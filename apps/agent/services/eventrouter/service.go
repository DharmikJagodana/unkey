package eventrouter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/unkeyed/unkey/apps/agent/pkg/auth"
	"github.com/unkeyed/unkey/apps/agent/pkg/batch"
	"github.com/unkeyed/unkey/apps/agent/pkg/logging"
	"github.com/unkeyed/unkey/apps/agent/pkg/metrics"
	"github.com/unkeyed/unkey/apps/agent/pkg/prometheus"
	"github.com/unkeyed/unkey/apps/agent/pkg/tinybird"
	"github.com/unkeyed/unkey/apps/agent/pkg/tracing"
)

type event struct {
	datasource string
	row        any
}

type Config struct {
	BatchSize     int
	BufferSize    int
	FlushInterval time.Duration

	Tinybird  *tinybird.Client
	Logger    logging.Logger
	Metrics   metrics.Metrics
	AuthToken string
}

type Service struct {
	logger    logging.Logger
	metrics   metrics.Metrics
	batcher   batch.BatchProcessor[event]
	tb        *tinybird.Client
	authToken string
}

func New(config Config) (*Service, error) {

	flush := func(ctx context.Context, events []event) {
		if len(events) == 0 {
			return
		}
		// config.Metrics.RecordFlush()
		eventsByDatasource := map[string][]any{}
		for _, e := range events {
			if _, ok := eventsByDatasource[e.datasource]; !ok {
				eventsByDatasource[e.datasource] = []any{}
			}
			eventsByDatasource[e.datasource] = append(eventsByDatasource[e.datasource], e.row)
		}
		for datasource, rows := range eventsByDatasource {
			err := config.Tinybird.Ingest(datasource, rows)
			if err != nil {
				config.Logger.Err(err).Str("datasource", datasource).Int("rows", len(rows)).Msg("Error ingesting")
			}
			prometheus.EventRouterFlushedRows.With(map[string]string{
				"datasource": datasource,
			}).Add(float64(len(rows)))

		}
	}

	batcher := batch.New(batch.Config[event]{
		BatchSize:     config.BatchSize,
		BufferSize:    config.BufferSize,
		FlushInterval: config.FlushInterval,
		Flush:         flush,
	})
	return &Service{
		logger:    config.Logger,
		metrics:   config.Metrics,
		batcher:   *batcher,
		tb:        config.Tinybird,
		authToken: config.AuthToken,
	}, nil
}

func (s *Service) CreateHandler() (string, http.Handler) {
	return "/v0/events", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			defer r.Body.Close()
		}

		ctx, span := tracing.Start(r.Context(), tracing.NewSpanName("eventrouter", "v0/events"))
		defer span.End()

		err := auth.Authorize(ctx, s.authToken, r.Header.Get("Authorization"))
		if err != nil {
			s.logger.Warn().Err(err).Msg("failed to authorize request")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		datasource := r.URL.Query().Get("name")
		if datasource == "" {
			http.Error(w, "missing ?name=", http.StatusBadRequest)
			return
		}

		dec := json.NewDecoder(r.Body)

		rows := []any{}

		for {
			var v any
			err := dec.Decode(&v)
			if err != nil {
				if err == io.EOF {
					break
				}
				s.logger.Err(err).Msg("Error decoding row")
				http.Error(w, err.Error(), http.StatusBadRequest)
				break
			}
			rows = append(rows, v)
		}

		for _, row := range rows {
			s.batcher.Buffer(event{datasource, row})
		}

		response := tinybird.Response{
			SuccessfulRows:  len(rows),
			QuarantinedRows: 0,
		}
		responseBody, err := json.Marshal(response)
		if err != nil {
			s.logger.Err(err).Msg("Error marshalling response")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, err = w.Write(responseBody)
		if err != nil {
			s.logger.Err(err).Msg("Error writing response")
		}

	})
}
