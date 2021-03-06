// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package pump

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/mux"
	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	pd "github.com/pingcap/pd/client"
	"github.com/pingcap/tidb-binlog/pkg/flags"
	"github.com/pingcap/tidb-binlog/pkg/node"
	"github.com/pingcap/tidb-binlog/pkg/util"
	"github.com/pingcap/tidb-binlog/pump/storage"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tidb/store/tikv/oracle"
	"github.com/pingcap/tipb/go-binlog"
	pb "github.com/pingcap/tipb/go-binlog"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/soheilhy/cmux"
	"github.com/unrolled/render"
	"go.uber.org/zap"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	notifyDrainerTimeout     = time.Second * 10
	serverInfoOutputInterval = time.Second * 10
	// GlobalConfig is global config of pump
	GlobalConfig *globalConfig
)

// Server implements the gRPC interface,
// and maintains pump's status at run time.
type Server struct {
	dataDir string
	storage storage.Storage

	clusterID uint64

	// node maintains the status of this pump and interact with etcd registry
	node node.Node

	tcpAddr    string
	unixAddr   string
	gs         *grpc.Server
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	gcDuration time.Duration
	metrics    *util.MetricClient
	// save the last time we write binlog to Storage
	// if long time not write, we can write a fake binlog
	lastWriteBinlogUnixNano int64
	pdCli                   pd.Client
	cfg                     *Config

	writeBinlogCount int64
	alivePullerCount int64

	isClosed int32
}

func init() {
	// tracing has suspicious leak problem, so disable it here.
	// it must be set before any real grpc operation.
	grpc.EnableTracing = false
	GlobalConfig = &globalConfig{
		maxMsgSize: defautMaxKafkaSize,
	}
}

// NewServer returns a instance of pump server
func NewServer(cfg *Config) (*Server, error) {
	var metrics *util.MetricClient
	if cfg.MetricsAddr != "" && cfg.MetricsInterval != 0 {
		metrics = util.NewMetricClient(
			cfg.MetricsAddr,
			time.Duration(cfg.MetricsInterval)*time.Second,
			registry,
		)
	}

	// get pd client and cluster ID
	pdCli, err := util.GetPdClient(cfg.EtcdURLs, cfg.Security)
	if err != nil {
		return nil, errors.Trace(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	clusterID := pdCli.GetClusterID(ctx)
	log.Info("get clusterID success", zap.Uint64("clusterID", clusterID))

	grpcOpts := []grpc.ServerOption{grpc.MaxRecvMsgSize(GlobalConfig.maxMsgSize)}
	if cfg.tls != nil {
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(cfg.tls)))
	}

	urlv, err := flags.NewURLsValue(cfg.EtcdURLs)
	if err != nil {
		return nil, errors.Trace(err)
	}

	lockResolver, err := tikv.NewLockResolver(urlv.StringSlice(), cfg.Security.ToTiDBSecurityConfig())
	if err != nil {
		return nil, errors.Trace(err)
	}

	session.RegisterStore("tikv", tikv.Driver{})
	tiPath := fmt.Sprintf("tikv://%s?disableGC=true", urlv.HostString())
	tiStore, err := session.NewStore(tiPath)
	if err != nil {
		return nil, errors.Trace(err)
	}

	options := storage.DefaultOptions()
	options = options.WithKVConfig(cfg.Storage.KV)
	options = options.WithSync(cfg.Storage.GetSyncLog())

	storage, err := storage.NewAppendWithResolver(cfg.DataDir, options, tiStore, lockResolver)
	if err != nil {
		return nil, errors.Trace(err)
	}

	n, err := NewPumpNode(cfg, storage.MaxCommitTS)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &Server{
		dataDir:    cfg.DataDir,
		storage:    storage,
		clusterID:  clusterID,
		node:       n,
		unixAddr:   cfg.Socket,
		tcpAddr:    cfg.ListenAddr,
		gs:         grpc.NewServer(grpcOpts...),
		ctx:        ctx,
		cancel:     cancel,
		metrics:    metrics,
		gcDuration: time.Duration(cfg.GC) * 24 * time.Hour,
		pdCli:      pdCli,
		cfg:        cfg,
	}, nil
}

// WriteBinlog implements the gRPC interface of pump server
func (s *Server) WriteBinlog(ctx context.Context, in *binlog.WriteBinlogReq) (*binlog.WriteBinlogResp, error) {
	// pump client will write some empty Payload to detect whether pump is working, should avoid this
	if in.Payload == nil {
		ret := new(binlog.WriteBinlogResp)
		return ret, nil
	}

	atomic.AddInt64(&s.writeBinlogCount, 1)
	return s.writeBinlog(ctx, in, false)
}

// WriteBinlog implements the gRPC interface of pump server
func (s *Server) writeBinlog(ctx context.Context, in *binlog.WriteBinlogReq, isFakeBinlog bool) (*binlog.WriteBinlogResp, error) {
	var err error
	beginTime := time.Now()
	atomic.StoreInt64(&s.lastWriteBinlogUnixNano, beginTime.UnixNano())

	defer func() {
		var label string
		if err != nil {
			label = "fail"
		} else {
			label = "succ"
		}

		rpcHistogram.WithLabelValues("WriteBinlog", label).Observe(time.Since(beginTime).Seconds())
	}()

	if in.ClusterID != s.clusterID {
		err = errors.Errorf("cluster ID are mismatch, %v vs %v", in.ClusterID, s.clusterID)
		return nil, err
	}

	ret := new(binlog.WriteBinlogResp)

	blog := new(binlog.Binlog)
	err = blog.Unmarshal(in.Payload)
	if err != nil {
		goto errHandle
	}

	if !isFakeBinlog {
		state := s.node.NodeStatus().State
		if state != node.Online {
			err = errors.Errorf("no online: %v", state)
			goto errHandle
		}
	}

	err = s.storage.WriteBinlog(blog)
	if err != nil {
		goto errHandle
	}

	return ret, nil

errHandle:
	lossBinlogCacheCounter.Add(1)
	log.Error("write binlog failed", zap.Error(err))
	ret.Errmsg = err.Error()
	return ret, err
}

// PullBinlogs sends binlogs in the streaming way
func (s *Server) PullBinlogs(in *binlog.PullBinlogReq, stream binlog.Pump_PullBinlogsServer) error {
	var err error
	beginTime := time.Now()
	atomic.AddInt64(&s.alivePullerCount, 1)

	log.Debug("get PullBinlogs request", zap.Reflect("request", in))
	defer func() {
		log.Debug("PullBinlogs request quit", zap.Reflect("request", in))
		var label string
		if err != nil {
			label = "fail"
		} else {
			label = "succ"
		}

		atomic.AddInt64(&s.alivePullerCount, -1)
		rpcHistogram.WithLabelValues("PullBinlogs", label).Observe(time.Since(beginTime).Seconds())
	}()

	if in.ClusterID != s.clusterID {
		return errors.Errorf("cluster ID are mismatch, %v vs %v", in.ClusterID, s.clusterID)
	}

	// don't use pos.Suffix now, use offset like last commitTS
	last := in.StartFrom.Offset

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	binlogs := s.storage.PullCommitBinlog(ctx, last)

	for {
		select {
		case data, ok := <-binlogs:
			if !ok {
				return nil
			}
			resp := new(binlog.PullBinlogResp)

			resp.Entity.Payload = data
			err = stream.Send(resp)
			if err != nil {
				log.Warn("send failed", zap.Error(err))
				return err
			}
			log.Debug("PullBinlogs send binlog payload success", zap.Int("len", len(data)))
		case <-stream.Context().Done():
			log.Debug("stream done", zap.Error(stream.Context().Err()))
			return nil
		}
	}
}

func (s *Server) registerNode(ctx context.Context, state string, updateTS int64) error {
	n := s.node
	status := node.NewStatus(n.NodeStatus().NodeID, n.NodeStatus().Addr, state, 0, s.storage.MaxCommitTS(), updateTS)
	return n.RefreshStatus(ctx, status)
}

func (s *Server) startHeartbeat() {
	errc := s.node.Heartbeat(s.ctx)
	go func() {
		for err := range errc {
			if err != context.Canceled {
				log.Error("send heartbeat failed", zap.Error(err))
			}
		}
	}()
}

// Start runs Pump Server to serve the listening addr, and maintains heartbeat to Etcd
func (s *Server) Start() error {
	// register this node
	ts, err := s.getTSO()
	if err != nil {
		return errors.Annotate(err, "fail to get tso from pd")
	}
	if err := s.registerNode(context.Background(), node.Online, ts); err != nil {
		return errors.Annotate(err, "fail to register node to etcd")
	}

	log.Info("register success", zap.String("NodeID", s.node.NodeStatus().NodeID))

	// notify all cisterns
	ctx, _ := context.WithTimeout(s.ctx, notifyDrainerTimeout)
	if err := s.node.Notify(ctx); err != nil {
		// if fail, refresh this node's state to paused
		if err := s.registerNode(context.Background(), node.Paused, 0); err != nil {
			log.Error("unregister pump while pump fails to notify drainer", zap.Error(err))
		}
		return errors.Annotate(err, "fail to notify all living drainer")
	}

	log.Debug("notify success")

	s.startHeartbeat()

	// start a UNIX listener
	var unixLis net.Listener
	if s.unixAddr != "" {
		unixURL, err := url.Parse(s.unixAddr)
		if err != nil {
			return errors.Annotatef(err, "invalid listening socket addr (%s)", s.unixAddr)
		}
		unixLis, err = net.Listen("unix", unixURL.Path)
		if err != nil {
			return errors.Annotatef(err, "fail to start UNIX on %s", unixURL.Path)
		}
	}

	log.Debug("init success")

	// start a TCP listener
	tcpURL, err := url.Parse(s.tcpAddr)
	if err != nil {
		return errors.Annotatef(err, "invalid listening tcp addr (%s)", s.tcpAddr)
	}
	tcpLis, err := net.Listen("tcp", tcpURL.Host)
	if err != nil {
		return errors.Annotatef(err, "fail to start TCP listener on %s", tcpURL.Host)
	}

	// start generate binlog if pump doesn't receive new binlogs
	s.wg.Add(1)
	go s.genForwardBinlog()

	// gc old binlog files
	s.wg.Add(1)
	go s.gcBinlogFile()

	// collect metrics to prometheus
	s.wg.Add(1)
	go s.startMetrics()

	s.wg.Add(1)
	go s.printServerInfo()

	// register pump with gRPC server and start to serve listeners
	binlog.RegisterPumpServer(s.gs, s)

	if s.unixAddr != "" {
		go s.gs.Serve(unixLis)
	}

	// grpc and http will use the same tcp connection
	m := cmux.New(tcpLis)
	// sets a timeout for the read of matchers
	m.SetReadTimeout(time.Second * 10)

	grpcL := m.Match(cmux.HTTP2HeaderField("content-type", "application/grpc"))
	httpL := m.Match(cmux.HTTP1Fast())
	go s.gs.Serve(grpcL)

	router := mux.NewRouter()
	router.HandleFunc("/status", s.Status).Methods("GET")
	router.HandleFunc("/state/{nodeID}/{action}", s.ApplyAction).Methods("PUT")
	router.HandleFunc("/drainers", s.AllDrainers).Methods("GET")
	router.HandleFunc("/debug/binlog/{ts}", s.BinlogByTS).Methods("GET")
	http.Handle("/", router)
	prometheus.DefaultGatherer = registry
	http.Handle("/metrics", promhttp.Handler())

	go http.Serve(httpL, nil)

	log.Info("start to server request", zap.String("addr", s.tcpAddr))
	err = m.Serve()
	if strings.Contains(err.Error(), "use of closed network connection") {
		err = nil
	}

	return err
}

// gennerate rollback binlog can forward the drainer's latestCommitTs, and just be discarded without any side effects
func (s *Server) genFakeBinlog() (*pb.Binlog, error) {
	ts, err := s.getTSO()
	if err != nil {
		return nil, errors.Trace(err)
	}

	bl := &binlog.Binlog{
		StartTs:  ts,
		Tp:       binlog.BinlogType_Rollback,
		CommitTs: ts,
	}

	return bl, nil
}

func (s *Server) writeFakeBinlog() (*pb.Binlog, error) {
	binlog, err := s.genFakeBinlog()
	if err != nil {
		return nil, errors.Annotate(err, "gennerate fake binlog err")
	}

	payload, err := binlog.Marshal()
	if err != nil {
		return nil, errors.Annotate(err, "gennerate fake binlog err")
	}

	req := new(pb.WriteBinlogReq)
	req.Payload = payload
	req.ClusterID = s.clusterID

	resp, err := s.writeBinlog(s.ctx, req, true)
	if err != nil {
		return nil, errors.Annotate(err, "write fake binlog err")
	}

	if len(resp.Errmsg) > 0 {
		return nil, errors.Errorf("write fake binlog err: %v", resp.Errmsg)
	}

	log.Debug("write fake binlog successful")
	return binlog, nil
}

// we would generate binlog to forward the pump's latestCommitTs in drainer when there is no binlogs in this pump
func (s *Server) genForwardBinlog() {
	defer s.wg.Done()

	genFakeBinlogInterval := time.Duration(s.cfg.GenFakeBinlogInterval) * time.Second
	lastWriteBinlogUnixNano := atomic.LoadInt64(&s.lastWriteBinlogUnixNano)
	for {
		select {
		case <-s.ctx.Done():
			log.Info("genFakeBinlog exit")
			return
		case <-time.After(genFakeBinlogInterval):
			// if no WriteBinlogReq, we write a fake binlog
			if lastWriteBinlogUnixNano == atomic.LoadInt64(&s.lastWriteBinlogUnixNano) {
				_, err := s.writeFakeBinlog()
				if err != nil {
					log.Error("write fake binlog failed", zap.Error(err))
				}
			}
			lastWriteBinlogUnixNano = atomic.LoadInt64(&s.lastWriteBinlogUnixNano)
		}
	}
}

func (s *Server) printServerInfo() {
	defer s.wg.Done()

	ticker := time.NewTicker(serverInfoOutputInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			log.Info("printServerInfo exit")
			return
		case <-ticker.C:
			log.Info("server info tick",
				zap.Int64("writeBinlogCount", atomic.LoadInt64(&s.writeBinlogCount)),
				zap.Int64("alivePullerCount", atomic.LoadInt64(&s.alivePullerCount)),
				zap.Int64("MaxCommitTS", s.storage.MaxCommitTS()))
		}
	}
}

func (s *Server) gcBinlogFile() {
	defer s.wg.Done()

	for {
		select {
		case <-s.ctx.Done():
			log.Info("gcBinlogFile exit")
			return
		case <-time.After(time.Hour):
			if s.gcDuration == 0 {
				continue
			}

			safeTSO, err := s.getSaveGCTSOForDrainers()
			if err != nil {
				log.Warn("get save gc tso for drainers failed", zap.Error(err))
				continue
			}
			log.Info("get safe ts for drainers success", zap.Int64("ts", safeTSO))

			millisecond := time.Now().Add(-s.gcDuration).UnixNano() / 1000 / 1000
			gcTS := int64(oracle.EncodeTSO(millisecond))
			if safeTSO < gcTS {
				gcTS = safeTSO
			}
			log.Info("send gc request to storage", zap.Int64("ts", gcTS))
			s.storage.GCTS(gcTS)
		}
	}
}

func (s *Server) getSaveGCTSOForDrainers() (int64, error) {
	pumpNode := s.node.(*pumpNode)

	drainers, err := pumpNode.Nodes(s.ctx, "drainers")
	if err != nil {
		return 0, errors.Trace(err)
	}

	var minTSO int64 = math.MaxInt64
	for _, drainer := range drainers {
		if drainer.State == node.Offline {
			continue
		}

		if drainer.MaxCommitTS < minTSO {
			minTSO = drainer.MaxCommitTS
		}
	}

	return minTSO, nil
}

func (s *Server) startMetrics() {
	defer s.wg.Done()

	if s.metrics == nil {
		return
	}
	log.Info("start metricClient")
	s.metrics.Start(s.ctx, map[string]string{"instance": s.node.ID()})
	log.Info("startMetrics exit")
}

// AllDrainers exposes drainers' status to HTTP handler.
func (s *Server) AllDrainers(w http.ResponseWriter, r *http.Request) {
	node, ok := s.node.(*pumpNode)
	if !ok {
		json.NewEncoder(w).Encode("can't provide service")
		return
	}

	pumps, err := node.EtcdRegistry.Nodes(s.ctx, "drainers")
	if err != nil {
		log.Error("get pumps failed", zap.Error(err))
	}

	json.NewEncoder(w).Encode(pumps)
}

// Status exposes pumps' status to HTTP handler.
func (s *Server) Status(w http.ResponseWriter, r *http.Request) {
	s.PumpStatus().Status(w, r)
}

// BinlogByTS exposes api get get binlog by ts
func (s *Server) BinlogByTS(w http.ResponseWriter, r *http.Request) {
	tsStr := mux.Vars(r)["ts"]
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		w.Write([]byte(fmt.Sprintf("invalid parameter ts: %s", tsStr)))
		return
	}

	binlog, err := s.storage.GetBinlog(ts)
	if err != nil {
		w.Write([]byte(err.Error()))
		return
	}

	w.Write([]byte(binlog.String()))
	if len(binlog.PrewriteValue) > 0 {
		prewriteValue := new(pb.PrewriteValue)
		prewriteValue.Unmarshal(binlog.PrewriteValue)

		w.Write([]byte("\n\n PrewriteValue: \n"))
		w.Write([]byte(prewriteValue.String()))
	}
}

// PumpStatus returns all pumps' status.
func (s *Server) PumpStatus() *HTTPStatus {
	status, err := s.node.NodesStatus(s.ctx)
	if err != nil {
		log.Error("get pumps' status failed", zap.Error(err))
		return &HTTPStatus{
			ErrMsg: err.Error(),
		}
	}

	statusMap := make(map[string]*node.Status)
	for _, st := range status {
		statusMap[st.NodeID] = st
	}

	// get ts from pd
	commitTS, err := s.getTSO()
	if err != nil {
		log.Error("get ts from pd failed", zap.Error(err))
		return &HTTPStatus{
			ErrMsg: err.Error(),
		}
	}

	return &HTTPStatus{
		StatusMap: statusMap,
		CommitTS:  commitTS,
	}
}

// ChangeStateReq is the request struct for change state.
type ChangeStateReq struct {
	NodeID string `json:"nodeID"`
	State  string `json:"state"`
}

// ApplyAction change the pump's state, now can be pause or close.
func (s *Server) ApplyAction(w http.ResponseWriter, r *http.Request) {
	rd := render.New(render.Options{
		IndentJSON: true,
	})

	nodeID := mux.Vars(r)["nodeID"]
	action := mux.Vars(r)["action"]
	log.Info("receive action", zap.String("nodeID", nodeID), zap.String("action", action))

	if nodeID != s.node.NodeStatus().NodeID {
		rd.JSON(w, http.StatusOK, util.ErrResponsef("invalide nodeID %s, this pump's nodeID is %s", nodeID, s.node.NodeStatus().NodeID))
		return
	}

	if s.node.NodeStatus().State != node.Online {
		rd.JSON(w, http.StatusOK, util.ErrResponsef("this pump's state is %s, apply %s failed!", s.node.NodeStatus().State, action))
		return
	}

	switch action {
	case "pause":
		log.Info("pump's state change to pausing", zap.String("nodeID", nodeID))
		s.node.NodeStatus().State = node.Pausing
	case "close":
		log.Info("pump's state change to closing", zap.String("nodeID", nodeID))
		s.node.NodeStatus().State = node.Closing
	default:
		rd.JSON(w, http.StatusOK, util.ErrResponsef("invalide action %s", action))
		return
	}

	go s.Close()
	rd.JSON(w, http.StatusOK, util.SuccessResponse(fmt.Sprintf("apply action %s success!", action), nil))
}

var utilGetTSO = util.GetTSO

func (s *Server) getTSO() (int64, error) {
	ts, err := utilGetTSO(s.pdCli)
	if err != nil {
		return 0, errors.Trace(err)
	}

	return ts, nil
}

// return if and only if it's safe to offline or context is canceled
func (s *Server) waitSafeToOffline(ctx context.Context) error {
	var fakeBinlog *pb.Binlog
	var err error

	// write a fake binlog
	for {
		fakeBinlog, err = s.writeFakeBinlog()
		if err != nil {
			log.Error("write fake binlog failed", zap.Error(err))

			select {
			case <-time.After(time.Second):
				continue
			case <-ctx.Done():
				return errors.Trace(ctx.Err())
			}
		}

		break
	}

	// check storage has handle this fake binlog
	for {
		maxCommitTS := s.storage.MaxCommitTS()
		if maxCommitTS < fakeBinlog.CommitTs {
			log.Info("max commit TS in storage less than fake binlog commit ts",
				zap.Int64("max commit ts", maxCommitTS),
				zap.Int64("fake binlog commit ts", fakeBinlog.CommitTs))
			select {
			case <-time.After(time.Second):
				continue
			case <-ctx.Done():
				return errors.Trace(ctx.Err())
			}
		}

		break
	}

	log.Debug("start to check offline safe for drainers")

	// check drainer has consume fake binlog we just write
	for {
		select {
		case <-time.After(time.Second):
			pumpNode := s.node.(*pumpNode)
			drainers, err := pumpNode.Nodes(ctx, "drainers")
			if err != nil {
				log.Error("get nodes failed", zap.Error(err))
				break
			}
			needByDrainer := false
			for _, drainer := range drainers {
				if drainer.State == node.Offline {
					continue
				}

				if drainer.MaxCommitTS < fakeBinlog.CommitTs {
					log.Info("wait for drainer to consume binlog",
						zap.String("drainer NodeID", drainer.NodeID),
						zap.Int64("drainer MaxCommitTS", drainer.MaxCommitTS),
						zap.Int64("Binlog CommiTS", fakeBinlog.CommitTs))
					needByDrainer = true
					break
				}
			}
			if !needByDrainer {
				return nil
			}

			_, err = s.writeFakeBinlog()
			if err != nil {
				log.Error("write fake binlog failed", zap.Error(err))
			}

		case <-ctx.Done():
			return errors.Trace(ctx.Err())
		}
	}
}

// commitStatus commit the node's last status to pd when close the server.
func (s *Server) commitStatus() {
	// update this node
	var state string
	switch s.node.NodeStatus().State {
	case node.Pausing, node.Online:
		state = node.Paused
	case node.Closing:
		s.waitSafeToOffline(context.Background())
		log.Info("safe to offline now")
		state = node.Offline
	default:
		log.Warn("there must be something wrong", zap.Reflect("status", s.node.NodeStatus()))
		state = s.node.NodeStatus().State
	}
	if err := s.registerNode(context.Background(), state, 0); err != nil {
		log.Error("unregister pump failed", zap.Error(err))
	}
	log.Info("update state success",
		zap.String("NodeID", s.node.NodeStatus().NodeID),
		zap.String("state", state))
}

// Close gracefully releases resource of pump server
func (s *Server) Close() {
	log.Info("begin to close pump server")
	if !atomic.CompareAndSwapInt32(&s.isClosed, 0, 1) {
		log.Debug("server had closed")
		return
	}

	// notify other goroutines to exit
	s.cancel()
	s.wg.Wait()
	log.Info("background goroutins are stopped")

	s.commitStatus()
	log.Info("commit status done")

	// stop the gRPC server
	s.gs.GracefulStop()
	log.Info("grpc is stopped")

	if err := s.storage.Close(); err != nil {
		log.Error("close storage failed", zap.Error(err))
	}
	log.Info("storage is closed")

	if err := s.node.Quit(); err != nil {
		log.Error("close pump node failed", zap.Error(err))
	}
	log.Info("pump node is closed")

	// close tiStore
	if s.pdCli != nil {
		s.pdCli.Close()
	}
	log.Info("has closed pdCli")
}
