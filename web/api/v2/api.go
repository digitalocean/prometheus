// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api_v2

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/v3/pkg/timestamp"
	pb "github.com/prometheus/prometheus/v3/prompb"
	"github.com/prometheus/tsdb"
	tsdbLabels "github.com/prometheus/tsdb/labels"
	old_ctx "golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const commitChunkSize = 500

var (
	remoteWriteAppendFailure = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "prometheus_remote_write_append_failures",
			Help: "How many samples failed to append to tsdb from remote write requests",
		}, []string{"reason"},
	)
)

func init() {
	prometheus.MustRegister(remoteWriteAppendFailure)
}

// API encapsulates all API services.
type API struct {
	enableAdmin bool
	db          func() *tsdb.DB
	logger      log.Logger
}

// New returns a new API object.
func New(
	db func() *tsdb.DB,
	enableAdmin bool,
	logger log.Logger,
) *API {
	return &API{
		db:          db,
		enableAdmin: enableAdmin,
		logger:      logger,
	}
}

// RegisterGRPC registers all API services with the given server.
func (api *API) RegisterGRPC(srv *grpc.Server) {
	if api.enableAdmin {
		pb.RegisterAdminServer(srv, NewAdmin(api.db, api.logger))
	} else {
		pb.RegisterAdminServer(srv, &AdminDisabled{})
	}
}

// HTTPHandler returns an HTTP handler for a REST API gateway to the given grpc address.
func (api *API) HTTPHandler(ctx context.Context, grpcAddr string) (http.Handler, error) {
	enc := new(protoutil.JSONPb)
	mux := runtime.NewServeMux(runtime.WithMarshalerOption(enc.ContentType(), enc))

	opts := []grpc.DialOption{
		grpc.WithInsecure(),
		// Replace the default dialer that connects through proxy when HTTP_PROXY is set.
		grpc.WithDialer(func(addr string, _ time.Duration) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}),
	}

	err := pb.RegisterAdminHandlerFromEndpoint(ctx, mux, grpcAddr, opts)
	if err != nil {
		return nil, err
	}
	return mux, nil
}

// extractTimeRange returns minimum and maximum timestamp in milliseconds as
// provided by the time range. It defaults either boundary to the minimum and maximum
// possible value.
func extractTimeRange(min, max *time.Time) (mint, maxt time.Time, err error) {
	if min == nil {
		mint = minTime
	} else {
		mint = *min
	}
	if max == nil {
		maxt = maxTime
	} else {
		maxt = *max
	}
	if mint.After(maxt) {
		return mint, maxt, errors.Errorf("min time must be before or equal to max time")
	}
	return mint, maxt, nil
}

var (
	minTime = time.Unix(math.MinInt64/1000+62135596801, 0)
	maxTime = time.Unix(math.MaxInt64/1000-62135596801, 999999999)
)

var (
	errAdminDisabled = status.Error(codes.Unavailable, "Admin APIs are disabled")
	errTSDBNotReady  = status.Error(codes.Unavailable, "TSDB not ready")
)

// AdminDisabled implements the administration interface that informs
// that the API endpoints are disabled.
type AdminDisabled struct {
}

// TSDBSnapshot implements pb.AdminServer.
func (s *AdminDisabled) TSDBSnapshot(_ old_ctx.Context, _ *pb.TSDBSnapshotRequest) (*pb.TSDBSnapshotResponse, error) {
	return nil, errAdminDisabled
}

// TSDBCleanTombstones implements pb.AdminServer.
func (s *AdminDisabled) TSDBCleanTombstones(_ old_ctx.Context, _ *pb.TSDBCleanTombstonesRequest) (*pb.TSDBCleanTombstonesResponse, error) {
	return nil, errAdminDisabled
}

// DeleteSeries implements pb.AdminServer.
func (s *AdminDisabled) DeleteSeries(_ old_ctx.Context, r *pb.SeriesDeleteRequest) (*pb.SeriesDeleteResponse, error) {
	return nil, errAdminDisabled
}

// RemoteWrite implements pb.AdminServer
func (s *AdminDisabled) RemoteWrite(stream pb.Admin_RemoteWriteServer) error { return errAdminDisabled }

// Admin provides an administration interface to Prometheus.
type Admin struct {
	db     func() *tsdb.DB
	logger log.Logger
}

// NewAdmin returns a Admin server.
func NewAdmin(db func() *tsdb.DB, logger log.Logger) *Admin {
	return &Admin{
		db:     db,
		logger: logger,
	}
}

// TSDBSnapshot implements pb.AdminServer.
func (s *Admin) TSDBSnapshot(_ old_ctx.Context, req *pb.TSDBSnapshotRequest) (*pb.TSDBSnapshotResponse, error) {
	db := s.db()
	if db == nil {
		return nil, errTSDBNotReady
	}
	var (
		snapdir = filepath.Join(db.Dir(), "snapshots")
		name    = fmt.Sprintf("%s-%x",
			time.Now().UTC().Format("20060102T150405Z0700"),
			rand.Int())
		dir = filepath.Join(snapdir, name)
	)
	if err := os.MkdirAll(dir, 0777); err != nil {
		return nil, status.Errorf(codes.Internal, "created snapshot directory: %s", err)
	}
	if err := db.Snapshot(dir, !req.SkipHead); err != nil {
		return nil, status.Errorf(codes.Internal, "create snapshot: %s", err)
	}
	return &pb.TSDBSnapshotResponse{Name: name}, nil
}

// TSDBCleanTombstones implements pb.AdminServer.
func (s *Admin) TSDBCleanTombstones(_ old_ctx.Context, _ *pb.TSDBCleanTombstonesRequest) (*pb.TSDBCleanTombstonesResponse, error) {
	db := s.db()
	if db == nil {
		return nil, errTSDBNotReady
	}

	if err := db.CleanTombstones(); err != nil {
		return nil, status.Errorf(codes.Internal, "clean tombstones: %s", err)
	}

	return &pb.TSDBCleanTombstonesResponse{}, nil
}

// DeleteSeries implements pb.AdminServer.
func (s *Admin) DeleteSeries(_ old_ctx.Context, r *pb.SeriesDeleteRequest) (*pb.SeriesDeleteResponse, error) {
	mint, maxt, err := extractTimeRange(r.MinTime, r.MaxTime)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	var matchers tsdbLabels.Selector

	for _, m := range r.Matchers {
		var lm tsdbLabels.Matcher
		var err error

		switch m.Type {
		case pb.LabelMatcher_EQ:
			lm = tsdbLabels.NewEqualMatcher(m.Name, m.Value)
		case pb.LabelMatcher_NEQ:
			lm = tsdbLabels.Not(tsdbLabels.NewEqualMatcher(m.Name, m.Value))
		case pb.LabelMatcher_RE:
			lm, err = tsdbLabels.NewRegexpMatcher(m.Name, m.Value)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "bad regexp matcher: %s", err)
			}
		case pb.LabelMatcher_NRE:
			lm, err = tsdbLabels.NewRegexpMatcher(m.Name, m.Value)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument, "bad regexp matcher: %s", err)
			}
			lm = tsdbLabels.Not(lm)
		default:
			return nil, status.Error(codes.InvalidArgument, "unknown matcher type")
		}

		matchers = append(matchers, lm)
	}
	db := s.db()
	if db == nil {
		return nil, errTSDBNotReady
	}
	if err := db.Delete(timestamp.FromTime(mint), timestamp.FromTime(maxt), matchers...); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.SeriesDeleteResponse{}, nil
}

// RemoteWrite receives a stream of write requests and performs a remote write action with them
func (s *Admin) RemoteWrite(stream pb.Admin_RemoteWriteServer) error {
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			level.Error(s.logger).Log("msg", "read from grpc stream failure", "err", err)
			return err
		}

		// Write all metrics sent
		WriteTimeSeries(resp.GetTimeseries(), s.db, s.logger)
	}
	return nil
}

// WriteTimeSeries writes a set of timeseries metrics to the tsdb
func WriteTimeSeries(timeseries []pb.TimeSeries, tsdb func() *tsdb.DB, logger log.Logger) {
	ap := tsdb().Appender()

	commit := func() {
		if err := ap.Commit(); err != nil {
			level.Error(logger).Log("msg", "failure trying to commit write to store", "err", err)
			if err := ap.Rollback(); err != nil {
				level.Error(logger).Log("msg", "failure trying to rollback write to store", "err", err)
			}
		}
	}
	defer commit()

	for i, ts := range timeseries {
		if i%commitChunkSize == 0 {
			commit()
		}
		lbls := make(tsdbLabels.Labels, len(ts.Labels))
		for i, l := range ts.Labels {
			lbls[i] = tsdbLabels.Label{
				Name:  l.GetName(),
				Value: l.GetValue(),
			}
		}
		// soring guarantees hash consistency
		sort.Sort(lbls)

		var ref uint64
		var err error
		for _, s := range ts.Samples {
			if ref == 0 {
				ref, err = ap.Add(lbls, s.GetTimestamp(), s.GetValue())
			} else {
				err = ap.AddFast(ref, s.GetTimestamp(), s.GetValue())
			}
			if err != nil {
				remoteWriteAppendFailure.WithLabelValues(err.Error()).Inc()
			}
		}
	}
}
