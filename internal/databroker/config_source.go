package databroker

import (
	"context"
	"encoding/base64"
	"sync"

	"github.com/pomerium/pomerium/config"
	"github.com/pomerium/pomerium/internal/hashutil"
	"github.com/pomerium/pomerium/internal/log"
	"github.com/pomerium/pomerium/internal/telemetry/metrics"
	"github.com/pomerium/pomerium/internal/telemetry/trace"
	"github.com/pomerium/pomerium/pkg/grpc"
	configpb "github.com/pomerium/pomerium/pkg/grpc/config"
	"github.com/pomerium/pomerium/pkg/grpc/databroker"
	"github.com/pomerium/pomerium/pkg/grpcutil"
)

// ConfigSource provides a new Config source that decorates an underlying config with
// configuration derived from the data broker.
type ConfigSource struct {
	mu               sync.RWMutex
	computedConfig   *config.Config
	underlyingConfig *config.Config
	dbConfigs        map[string]dbConfig
	updaterHash      uint64
	cancel           func()

	config.ChangeDispatcher
}

type dbConfig struct {
	*configpb.Config
	version uint64
}

// NewConfigSource creates a new ConfigSource.
func NewConfigSource(underlying config.Source, listeners ...config.ChangeListener) *ConfigSource {
	src := &ConfigSource{
		dbConfigs: map[string]dbConfig{},
	}
	for _, li := range listeners {
		src.OnConfigChange(li)
	}
	underlying.OnConfigChange(func(cfg *config.Config) {
		src.mu.Lock()
		src.underlyingConfig = cfg.Clone()
		src.mu.Unlock()

		src.rebuild(false)
	})
	src.underlyingConfig = underlying.GetConfig()
	src.rebuild(true)
	return src
}

// GetConfig gets the current config.
func (src *ConfigSource) GetConfig() *config.Config {
	src.mu.RLock()
	defer src.mu.RUnlock()

	return src.computedConfig
}

func (src *ConfigSource) rebuild(firstTime bool) {
	_, span := trace.StartSpan(context.Background(), "databroker.config_source.rebuild")
	defer span.End()

	src.mu.Lock()
	defer src.mu.Unlock()

	cfg := src.underlyingConfig.Clone()

	// start the updater
	src.runUpdater(cfg)

	seen := map[uint64]struct{}{}
	for _, policy := range cfg.Options.GetAllPolicies() {
		id, err := policy.RouteID()
		if err != nil {
			log.Warn().Err(err).
				Str("policy", policy.String()).
				Msg("databroker: invalid policy config, ignoring")
			return
		}
		seen[id] = struct{}{}
	}

	var additionalPolicies []config.Policy

	// add all the config policies to the list
	for id, cfgpb := range src.dbConfigs {
		cfg.Options.ApplySettings(cfgpb.Settings)
		var errCount uint64

		err := cfg.Options.Validate()
		if err != nil {
			metrics.SetDBConfigRejected(cfg.Options.Services, id, cfgpb.version, err)
			return
		}

		for _, routepb := range cfgpb.GetRoutes() {
			policy, err := config.NewPolicyFromProto(routepb)
			if err != nil {
				errCount++
				log.Warn().Err(err).
					Str("db_config_id", id).
					Msg("databroker: error converting protobuf into policy")
				continue
			}

			err = policy.Validate()
			if err != nil {
				errCount++
				log.Warn().Err(err).
					Str("db_config_id", id).
					Str("policy", policy.String()).
					Msg("databroker: invalid policy, ignoring")
				continue
			}

			routeID, err := policy.RouteID()
			if err != nil {
				errCount++
				log.Warn().Err(err).
					Str("db_config_id", id).
					Str("policy", policy.String()).
					Msg("databroker: cannot establish policy route ID, ignoring")
				continue
			}

			if _, ok := seen[routeID]; ok {
				errCount++
				log.Warn().Err(err).
					Str("db_config_id", id).
					Str("policy", policy.String()).
					Msg("databroker: duplicate policy detected, ignoring")
				continue
			}
			seen[routeID] = struct{}{}

			additionalPolicies = append(additionalPolicies, *policy)
		}
		metrics.SetDBConfigInfo(cfg.Options.Services, id, cfgpb.version, int64(errCount))
	}

	// add the additional policies here since calling `Validate` will reset them.
	cfg.Options.AdditionalPolicies = append(cfg.Options.AdditionalPolicies, additionalPolicies...)

	src.computedConfig = cfg
	if !firstTime {
		src.Trigger(cfg)
	}

	metrics.SetConfigInfo(cfg.Options.Services, "databroker", cfg.Checksum(), true)
}

func (src *ConfigSource) runUpdater(cfg *config.Config) {
	urls, err := cfg.Options.GetDataBrokerURLs()
	if err != nil {
		log.Fatal().Err(err).Send()
		return
	}

	sharedKey, _ := base64.StdEncoding.DecodeString(cfg.Options.SharedKey)
	connectionOptions := &grpc.Options{
		Addrs:                   urls,
		OverrideCertificateName: cfg.Options.OverrideCertificateName,
		CA:                      cfg.Options.CA,
		CAFile:                  cfg.Options.CAFile,
		RequestTimeout:          cfg.Options.GRPCClientTimeout,
		ClientDNSRoundRobin:     cfg.Options.GRPCClientDNSRoundRobin,
		WithInsecure:            cfg.Options.GRPCInsecure,
		ServiceName:             cfg.Options.Services,
		SignedJWTKey:            sharedKey,
	}
	h, err := hashutil.Hash(connectionOptions)
	if err != nil {
		log.Fatal().Err(err).Send()
	}
	// nothing changed, so don't restart the updater
	if src.updaterHash == h {
		return
	}
	src.updaterHash = h

	if src.cancel != nil {
		src.cancel()
		src.cancel = nil
	}

	cc, err := grpc.NewGRPCClientConn(connectionOptions)
	if err != nil {
		log.Error().Err(err).Msg("databroker: failed to create gRPC connection to data broker")
		return
	}

	client := databroker.NewDataBrokerServiceClient(cc)

	ctx := context.Background()
	ctx, src.cancel = context.WithCancel(ctx)

	syncer := databroker.NewSyncer("databroker", &syncerHandler{
		client: client,
		src:    src,
	}, databroker.WithTypeURL(grpcutil.GetTypeURL(new(configpb.Config))))
	go func() { _ = syncer.Run(ctx) }()
}

type syncerHandler struct {
	src    *ConfigSource
	client databroker.DataBrokerServiceClient
}

func (s *syncerHandler) GetDataBrokerServiceClient() databroker.DataBrokerServiceClient {
	return s.client
}

func (s *syncerHandler) ClearRecords(ctx context.Context) {
	s.src.mu.Lock()
	s.src.dbConfigs = map[string]dbConfig{}
	s.src.mu.Unlock()
}

func (s *syncerHandler) UpdateRecords(ctx context.Context, serverVersion uint64, records []*databroker.Record) {
	if len(records) == 0 {
		return
	}

	s.src.mu.Lock()
	for _, record := range records {
		if record.GetDeletedAt() != nil {
			delete(s.src.dbConfigs, record.GetId())
			continue
		}

		var cfgpb configpb.Config
		err := record.GetData().UnmarshalTo(&cfgpb)
		if err != nil {
			log.Warn().Err(err).Msg("databroker: error decoding config")
			delete(s.src.dbConfigs, record.GetId())
			continue
		}

		s.src.dbConfigs[record.GetId()] = dbConfig{&cfgpb, record.Version}
	}
	s.src.mu.Unlock()

	s.src.rebuild(false)
}
