// Copyright 2017 PingCAP, Inc.
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

package schedulers

import (
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tikv/pd/pkg/errs"
	"github.com/tikv/pd/server/core"
	"github.com/tikv/pd/server/schedule"
	"github.com/tikv/pd/server/schedule/filter"
	"github.com/tikv/pd/server/schedule/operator"
	"github.com/tikv/pd/server/schedule/opt"
	"github.com/tikv/pd/server/statistics"
	"go.uber.org/zap"
)

func init() {
	schedule.RegisterSliceDecoderBuilder(MultipleDimensionType, func(args []string) schedule.ConfigDecoder {
		return func(v interface{}) error {
			return nil
		}
	})
	schedule.RegisterScheduler(MultipleDimensionType, func(opController *schedule.OperatorController, storage *core.Storage, decoder schedule.ConfigDecoder) (schedule.Scheduler, error) {
		conf := initHotRegionScheduleConfig()
		if err := decoder(conf); err != nil {
			return nil, err
		}
		conf.storage = storage
		return newMultiDimensionScheduler(opController, conf), nil
	})
}

const (
	// MultipleDimensionName is balance hot region scheduler name.
	MultipleDimensionName = "balance-multiple-dimension-scheduler"
	// MultipleDimensionType is balance hot region scheduler type.
	MultipleDimensionType = "multiple-dimension-scheduler"

	balanceRatioConst = float64(0.1)
	loadStableThresholdConst = float64(0.2)
	allowedDeviation = float64(0.05)
)

type multiDimensionScheduler struct {
	name string
	*BaseScheduler
	sync.RWMutex
	leaderLimit uint64
	peerLimit   uint64
	types       []rwType
	r           *rand.Rand

	// config of hot scheduler
	conf *hotRegionSchedulerConfig

	hotSched          *hotScheduler

	pendings map[*pendingLoadInfluence]struct{}
	regionPendings map[uint64]*operator.Operator
	pendingSums map[uint64]loadInfluence

	minExpLoads []float64
	mode                     int
	balanceRatio			 float64
	relaxBalanceCondition bool
	splitTrigeCount int
	curBalancer *multiBalancer
	hasSplit     bool
	needInit     bool
}

func newMultiDimensionScheduler(opController *schedule.OperatorController, conf *hotRegionSchedulerConfig) *multiDimensionScheduler {
	base := NewBaseScheduler(opController)
	ret := &multiDimensionScheduler{
		name:          MultipleDimensionName,
		BaseScheduler: base,
		leaderLimit:   1,
		peerLimit:     1,
		types:         []rwType{write, read},
		r:             rand.New(rand.NewSource(time.Now().UnixNano())),
		conf:          conf,
		hotSched:      newHotScheduler(opController, conf),
		pendings:      make(map[*pendingLoadInfluence]struct{}),
		regionPendings: make(map[uint64]*operator.Operator),

		balanceRatio: 			balanceRatioConst,
	}

	ret.minExpLoads = []float64{
		hotWriteRegionMinFlowRate, hotWriteRegionMinKeyRate, hotWriteRegionMinKeyRate,
		hotReadRegionMinFlowRate, hotReadRegionMinKeyRate, hotReadRegionMinKeyRate,
	}
	return ret
}

func (h *multiDimensionScheduler) GetName() string {
	return h.name
}

func (h *multiDimensionScheduler) GetType() string {
	return MultipleDimensionType
}

func (h *multiDimensionScheduler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.conf.ServeHTTP(w, r)
}

func (h *multiDimensionScheduler) GetMinInterval() time.Duration {
	return minHotScheduleInterval
}
func (h *multiDimensionScheduler) GetNextInterval(interval time.Duration) time.Duration {
	return intervalGrow(h.GetMinInterval(), maxHotScheduleInterval, exponentialGrowth)
}

func (h *multiDimensionScheduler) IsScheduleAllowed(cluster opt.Cluster) bool {
	return h.allowBalanceLeader(cluster) || h.allowBalanceRegion(cluster)
}

func (h *multiDimensionScheduler) allowBalanceLeader(cluster opt.Cluster) bool {
	return h.OpController.OperatorCount(operator.OpHotRegion) < cluster.GetOpts().GetHotRegionScheduleLimit() &&
		h.OpController.OperatorCount(operator.OpLeader) < cluster.GetOpts().GetLeaderScheduleLimit()
}

func (h *multiDimensionScheduler) allowBalanceRegion(cluster opt.Cluster) bool {
	return h.OpController.OperatorCount(operator.OpHotRegion) < cluster.GetOpts().GetHotRegionScheduleLimit()
}

func (h *multiDimensionScheduler) Schedule(cluster opt.Cluster) []*operator.Operator {
	schedulerCounter.WithLabelValues(h.GetName(), "schedule").Inc()
	return h.dispatch(h.types[1], cluster)
}

func (h *multiDimensionScheduler) dispatch(typ rwType, cluster opt.Cluster) []*operator.Operator {
	h.Lock()
	defer h.Unlock()

	mode := cluster.GetOpts().GetHotSchedulerMode()
	if mode > 0 {
		return nil
	}

	h.balanceRatio = cluster.GetOpts().GetHotBalanceRatio()
	if h.relaxBalanceCondition {
		h.balanceRatio += allowedDeviation
	}

	h.summaryPendingInfluence()
	if h.shouldWaitPendingOps() {
		return nil
	}

	if h.curBalancer == nil || h.needInit {
		balancer := newMultiBalancer(h, cluster)
		h.curBalancer = balancer
		h.needInit = false
	}
	return h.curBalancer.solveMultiLoads()
}

// summaryPendingInfluence calculate the summary of pending Influence for each store
// and clean the region from regionInfluence if they have ended operator.
func (h *multiDimensionScheduler) summaryPendingInfluence() {
	h.pendingSums = summaryPendingLoadInfluence(h.pendings, h.hotSched.calcPendingWeight)
	h.gcRegionPendings()
}

// gcRegionPendings check the region whether it need to be deleted from regionPendings depended on whether it have
// ended operator
func (h *multiDimensionScheduler) gcRegionPendings() {
	for regionID, op := range h.regionPendings {
		if op != nil && op.IsEnd() {
			if time.Now().After(op.GetStartTime().Add(h.conf.GetMaxZombieDuration())) {
				schedulerStatus.WithLabelValues(h.GetName(), "pending_op_infos").Dec()
				delete(h.regionPendings, regionID)
			}
		}
	}
}

func (h *multiDimensionScheduler) addPendingInfluence(op *operator.Operator, deci *decision, infl loadInfluence) bool {
	regionID := op.RegionID()
	_, ok := h.regionPendings[regionID]
	if ok {
		schedulerStatus.WithLabelValues(h.GetName(), "pending_op_fails").Inc()
		return false
	}

	influence := newPendingLoadInfluence(op, deci.srcStoreID, deci.dstStoreID, infl)
	h.pendings[influence] = struct{}{}
	h.regionPendings[regionID] = op

	schedulerStatus.WithLabelValues(h.GetName(), "pending_op_create").Inc()
	return true
}

func (h *multiDimensionScheduler) shouldWaitPendingOps() bool {
	if len(h.regionPendings) == 0 {
		h.hasSplit = false
		h.needInit = true
		log.Info("wakeup scheduler, no pending ops")
	} else if h.relaxBalanceCondition || h.hasSplit {
		return true
	}
	
	return false
}

type decision struct {
	srcStoreID  uint64
	dstStoreID  uint64

	opTy opType

	srcPeerStat *statistics.HotPeerStat
	region      *core.RegionInfo
	peer        *peerInfo
}

type multiBalancer struct {
	sche         *multiDimensionScheduler
	cluster      opt.Cluster

	cur *decision

	storeInfos   []*storeInfo
	allowedDimensions  []uint64
	allowedMap map[uint64]struct{}
	splitCandidates map[uint64][]*peerInfo
	scheduledRegions map[uint64]struct{}
	skipSchedule bool
}

func newMultiBalancer(sche *multiDimensionScheduler, cluster opt.Cluster) *multiBalancer {
	balancer := &multiBalancer{
		sche:    sche,
		cluster: cluster,
		scheduledRegions: make(map[uint64]struct{}),
	}

	balancer.initHotPeerInfo()

	return balancer
}

func (balancer *multiBalancer) needSkipSchedule(expLoads []float64) bool {
	balancer.skipSchedule = true
	for i := range expLoads {
		if i == 2 || i == 5 {
			continue
		}
		if expLoads[i] >= balancer.sche.minExpLoads[i] {
			balancer.skipSchedule = false
			balancer.allowedDimensions = append(balancer.allowedDimensions, uint64(i))
		}
	}

	balancer.allowedMap = make(map[uint64]struct{})
	for _, id := range balancer.allowedDimensions {
		balancer.allowedMap[id] = struct{}{}
	}

	return balancer.skipSchedule
}

func (balancer *multiBalancer) checkPendingLoads(storeLoads []map[uint64]float64) {
	storePendings := balancer.sche.pendingSums

	tyStrs := []string{
		"write-byte-rate-pending-ratio", "write-key-rate-pending-ratio", "write-ops-pending-ratio",
		"read-byte-rate-pending-ratio", "read-key-rate-pending-ratio", "read-ops-pending-ratio"}
	
	loadTyStrs := []string{
		"write-byte-rate-adjust", "write-key-rate-adjust", "write-ops-adjust",
		"read-byte-rate-adjust", "read-key-rate-adjust", "read-ops-adjust"}
	
	for id := range storeLoads[0] {
		infl := storePendings[id]
		for i := range tyStrs {
			if _, ok := balancer.allowedMap[uint64(i)]; !ok {
				continue
			}

			ratio := infl.loads[i] / storeLoads[i][id]
			hotPeerSummary.WithLabelValues(tyStrs[i], fmt.Sprintf("%v", id)).Set(ratio)
			hotPeerSummary.WithLabelValues(loadTyStrs[i], fmt.Sprintf("%v", id)).Set(storeLoads[i][id])
		}
	}
}

func (balancer *multiBalancer) initHotPeerInfo() {
	minHotDegree := balancer.cluster.GetOpts().GetHotRegionCacheHitsThreshold()
	storesStat := balancer.cluster.GetStoresStats()
	storeHotPeers := balancer.cluster.RegionWriteStats()

	storeLoads := storesStat.GetStoresLoadsStat()	// contains write bytes, write keys, write ops, read bytes, read keys, read ops
	expLoads := make([]float64, len(storeLoads))
	storeLen := len(storeLoads[0])
	storePendings := balancer.sche.pendingSums

	kind := core.RegionKind
	hotRegionThreshold := getHotRegionThreshold(storesStat, write)
	hotPeerFilterTy := mixed

	filteredStoreHotPeers := make(map[uint64][]*statistics.HotPeerStat)
	// Stores without byte rate statistics is not available to schedule.
	for id := range storeLoads[0] {
		// Find all hot peers first
		hotPeers := make([]*statistics.HotPeerStat, 0)
		for _, peer := range filterHotPeers(kind, minHotDegree, hotRegionThreshold, storeHotPeers[id], hotPeerFilterTy) {
			hotPeers = append(hotPeers, peer.Clone())
		}
		filteredStoreHotPeers[id] = hotPeers
		
		for i := range expLoads {
			expLoads[i] += storeLoads[i][id]
		}

		infl := storePendings[id]

		for i := range storeLoads {
			storeLoads[i][id] += infl.loads[i]
		}
	}

	for i := range expLoads {
		expLoads[i] /= float64(storeLen)
	}

	if balancer.needSkipSchedule(expLoads) {
		return
	}
	balancer.checkPendingLoads(storeLoads)

	maxLoadDiffRatio := 0.0
	balancer.storeInfos = make([]*storeInfo, 0, storeLen)
	for storeID, originHotPeers := range filteredStoreHotPeers {
		hotPeers := make(map[uint64]*peerInfo)
		hotPeersTotalLoads := make([]float64, len(expLoads))
		for _, originPeer := range originHotPeers {
			peer := newPeerInfo(originPeer.RegionID, storeID)
			originLoads := originPeer.GetLoads()
			for i := range expLoads {
				peer.loads[i] = originLoads[i] / expLoads[i]
				if !originPeer.IsLeader() && i >= 3 {
					peer.loads[i] = 0
				}
				hotPeersTotalLoads[i] += peer.loads[i] * expLoads[i] / 1024.0
			}
			peer.peerStat = originPeer
			peer.isLeader = originPeer.IsLeader()
			hotPeers[originPeer.RegionID] = peer
		}

		si := newStoreInfo(storeID, hotPeers)
		storeTotalLoads := make([]float64, len(expLoads))
		for i := range expLoads {
			si.loads[i] = storeLoads[i][storeID] / expLoads[i]
			storeTotalLoads[i] = storeLoads[i][storeID] / 1024.0

			if _, ok := balancer.allowedMap[uint64(i)]; ok {
				ratio := math.Abs((storeTotalLoads[i]-hotPeersTotalLoads[i])/storeTotalLoads[i])
				maxLoadDiffRatio = math.Max(maxLoadDiffRatio, ratio)
			}
		}
		balancer.storeInfos = append(balancer.storeInfos, si)

		log.Info("load info",
			zap.Uint64("storeID", storeID),
			zap.String("total(K)", fmt.Sprintf("%+v", storeTotalLoads)),
			zap.String("approximate(K)", fmt.Sprintf("%+v", hotPeersTotalLoads)),
		)
	}

	if maxLoadDiffRatio > loadStableThresholdConst {
		log.Info("load not stable",
			zap.Float64("maxLoadDiffRatio", maxLoadDiffRatio),
		)
		balancer.skipSchedule = true
		balancer.sche.needInit = true
	}
}

// filterDstStores select the candidate store by filters
func (balancer *multiBalancer) getCandidateStoreIDs(opTy opType) map[uint64]struct{} {
	selectedStores := make(map[uint64]struct{})
	srcStore := balancer.cluster.GetStore(balancer.cur.srcStoreID)
	if srcStore == nil {
		return selectedStores
	}

	var (
		filters []filter.Filter
		candidates []*core.StoreInfo
	)

	switch opTy {
	case movePeer:
		filters = []filter.Filter{
			filter.StoreStateFilter{ActionScope: balancer.sche.GetName(), MoveRegion: true},
			filter.NewExcludedFilter(balancer.sche.GetName(), balancer.cur.region.GetStoreIds(), balancer.cur.region.GetStoreIds()),
			filter.NewSpecialUseFilter(balancer.sche.GetName(), filter.SpecialUseHotRegion),
			filter.NewPlacementSafeguard(balancer.sche.GetName(), balancer.cluster, balancer.cur.region, srcStore),
		}

		candidates = balancer.cluster.GetStores()

	case transferLeader:
		filters = []filter.Filter{
			filter.StoreStateFilter{ActionScope: balancer.sche.GetName(), TransferLeader: true},
			filter.NewSpecialUseFilter(balancer.sche.GetName(), filter.SpecialUseHotRegion),
		}
		if leaderFilter := filter.NewPlacementLeaderSafeguard(balancer.sche.GetName(), balancer.cluster, balancer.cur.region, srcStore); leaderFilter != nil {
			filters = append(filters, leaderFilter)
		}

		candidates = balancer.cluster.GetFollowerStores(balancer.cur.region)

	default:
		return selectedStores
	}

	for _, store := range candidates {
		if filter.Target(balancer.cluster.GetOpts(), store, filters) {
			selectedStores[store.GetID()] = struct{}{}
		}
	}
	return selectedStores
}

func (balancer *multiBalancer) loadOfMigrated(store *storeInfo, opTy opType) float64 {
	maxLoad := 0.0
	for _, i := range balancer.allowedDimensions {
		if opTy == transferLeader && !loadCanTransfered(i) {	// skip transfer leader to write dimension
			continue
		}
		load := store.loads[i] + balancer.cur.peer.loads[i]
		if maxLoad < load {
			maxLoad = load
		}
	}
	return maxLoad
}

func (balancer *multiBalancer) filterDstStores(opTy opType, isLargeRegion bool) (dstStore *storeInfo, minLoad float64) {
	minLoad = math.MaxFloat64
	selectedStores := balancer.getCandidateStoreIDs(opTy)

	for _, store := range balancer.storeInfos {
		if _, ok := selectedStores[store.id]; !ok {
			continue
		}

		newLoad := balancer.loadOfMigrated(store, opTy)

		if newLoad <= 1 + balancer.sche.balanceRatio || !isLargeRegion {
			if newLoad < minLoad {
				dstStore = store
				minLoad = newLoad
			}
		}
	}
	return
}

func (balancer *multiBalancer) pickBestDstStore(targetDim uint64) *storeInfo {
	var (
		dstStore, dstStorePeer *storeInfo	
		minLoad, minLoadPeer float64
	)

	isLargeRegion := false
	for _, i := range balancer.allowedDimensions {
		if balancer.cur.peer.loads[i] > balancer.sche.balanceRatio {
			isLargeRegion = true
		}
	}

	minLoad = math.MaxFloat64
	if balancer.cur.peer.isLeader && loadCanTransfered(targetDim) {	// for read transfer leader
		dstStore, minLoad = balancer.filterDstStores(transferLeader, isLargeRegion)
		if dstStore != nil {
			balancer.cur.opTy = transferLeader
			balancer.cur.dstStoreID = dstStore.id
		}
	}

	dstStorePeer, minLoadPeer = balancer.filterDstStores(movePeer, isLargeRegion)
	if minLoadPeer < minLoad {
		dstStore = dstStorePeer
		balancer.cur.opTy = movePeer
		balancer.cur.dstStoreID = dstStore.id
	}
	return dstStore
}

func (balancer *multiBalancer) solveMultiLoads() []*operator.Operator {
	if balancer.skipSchedule {
		return nil
	}

	balancer.cur = &decision{}

	{
		var maxDimID uint64
		var maxLoad float64
		for _, si := range balancer.storeInfos {
			for _, i := range balancer.allowedDimensions {
				if maxLoad < si.loads[i] {
					maxLoad = si.loads[i]
					maxDimID = i
				}
			}
		}

		sort.Slice(balancer.storeInfos, func(i, j int) bool {
			return balancer.storeInfos[i].loads[maxDimID] > balancer.storeInfos[j].loads[maxDimID]
		})
	}
	
	log.Info("run solve",
		zap.String("allowedDimensions", fmt.Sprintf("%+v", balancer.allowedDimensions)),
	)

	for _, store := range balancer.storeInfos {
		log.Info("store load",
			zap.Uint64("id", store.id),
			zap.String("storeLoad", fmt.Sprintf("%+v", store.loads)),
		)
	}

	balancer.splitCandidates = make(map[uint64][]*peerInfo)

	for _, store := range balancer.storeInfos {
		maxID, maxLoad := store.getMaxLoadInfo(balancer.allowedDimensions)
		if maxLoad <= 1 + balancer.sche.balanceRatio {
			continue
		}		

		if balancer.sche.relaxBalanceCondition {
			balancer.sche.balanceRatio = balancer.cluster.GetOpts().GetHotBalanceRatio()
			balancer.sche.relaxBalanceCondition = false
		}
		
		sortedPeers := buildSortedPeers(store, maxID)
		log.Info("check loads",
			zap.Uint64("curDimID", maxID),
			zap.Float64("maxLoad", maxLoad),
			zap.Float64("remainLoad", sortedPeers.remainLoads),
		)
		for selectedPeer := sortedPeers.pop(); 
			selectedPeer != nil; 
			selectedPeer = sortedPeers.pop() {
			if _, ok := balancer.scheduledRegions[selectedPeer.regionID]; ok {
				log.Info("filter pending region",
					zap.Uint64("storeID", store.id),
					zap.Uint64("regionID", selectedPeer.regionID),
					zap.String("storeLoad", fmt.Sprintf("%+v", store.loads)),
				)
				continue
			}

			remainLoad := selectedPeer.loads[maxID] + sortedPeers.remainLoads
			// skip useless scheduling
			if remainLoad < balancer.sche.balanceRatio || remainLoad < (maxLoad - 1) * 0.8 {
				log.Info("skip useless scheduling",
					zap.String("regionLoad", fmt.Sprintf("%+v", selectedPeer.loads)),
					zap.Float64("remainLoad", sortedPeers.remainLoads),
					zap.Float64("maxLoad", maxLoad),
				)
				break
			}
			
			if maxLoad - selectedPeer.loads[maxID] < 1 - balancer.sche.balanceRatio {
				balancer.splitCandidates[store.id] = append(balancer.splitCandidates[store.id], selectedPeer)
				continue
			} else {
				balancer.cur.srcStoreID = store.id
				balancer.cur.srcPeerStat = selectedPeer.peerStat
				balancer.cur.region = balancer.getRegion(selectedPeer.regionID)
				balancer.cur.peer = selectedPeer
				if balancer.cur.region == nil {
					log.Info("no region",
						zap.Uint64("regionID", selectedPeer.regionID),
					)
					continue
				}

				dstStore := balancer.pickBestDstStore(maxID)
				if dstStore == nil { // there is no suitable place,  consider next region
					log.Info("no suitable store",
						zap.Uint64("regionID", selectedPeer.regionID),
						zap.Uint64("srcStoreID", store.id),
						zap.String("regionLoad", fmt.Sprintf("%+v", selectedPeer.loads)),
						zap.String("srdStoreLoad", fmt.Sprintf("%+v", store.loads)),
						zap.Uint64("balanceWhichLoad", maxID),
					)
					balancer.splitCandidates[store.id] = append(balancer.splitCandidates[store.id], selectedPeer)
					continue
				}
				
				log.Info("find placement",
					zap.Uint64("regionID", selectedPeer.regionID),
					zap.Uint64("srcStoreID", store.id),
					zap.Uint64("dstStoreID", dstStore.id),
					zap.String("regionLoad", fmt.Sprintf("%+v", selectedPeer.loads)),
					zap.String("srdStoreLoad", fmt.Sprintf("%+v", store.loads)),
					zap.String("dstStoreLoad", fmt.Sprintf("%+v", dstStore.loads)),
					zap.Uint64("balanceWhichLoad", maxID),
				)
				
				ops, infls := balancer.buildOperators()
				if ops == nil {
					log.Info("build operation failed",
						zap.Uint64("regionID", selectedPeer.regionID),
					)
				}

				for i := 0; i < len(ops); i++ {
					// TODO: multiple operators need to be atomic.
					if !balancer.sche.addPendingInfluence(ops[i], balancer.cur, infls[i]) {
						return nil
					}
				}

				migratePeer(store, dstStore, selectedPeer, balancer.cur.opTy)
				balancer.scheduledRegions[selectedPeer.regionID] = struct{}{}

				balancer.sche.splitTrigeCount = 0
				return ops
			}
		}

		log.Info("no candi region",
			zap.Uint64("storeID", store.id),
			zap.String("storeLoad", fmt.Sprintf("%+v", store.loads)),
		)
	}

	// relax balance condition to avoid flow deviation's influence
	ratio := calcBalanceRatio(balancer.storeInfos, balancer.allowedDimensions)
	if !balancer.sche.relaxBalanceCondition && ratio <= 1 + balancer.sche.balanceRatio + allowedDeviation {
		balancer.sche.relaxBalanceCondition = true
		balancer.sche.balanceRatio += allowedDeviation
		log.Info("relax balance condition")
	}

	if ratio > 1 + balancer.sche.balanceRatio { 	//  && len(balancer.sche.regionPendings) == 0
		return balancer.processSplit()
	}

	return nil
}

func (balancer *multiBalancer) processSplit() []*operator.Operator {
	var retOps []*operator.Operator

	log.Info("try split")

	balancer.sche.splitTrigeCount++
	if balancer.sche.splitTrigeCount == 5 { // && split op finished
		for _, store := range balancer.storeInfos {
			if candidates, ok := balancer.splitCandidates[store.id]; ok {
				maxID, maxLoad := store.getMaxLoadInfo(balancer.allowedDimensions)
				if maxLoad <= 1 + balancer.sche.balanceRatio {
					continue
				}

				loadThreshold := maxLoad - 1 - balancer.sche.balanceRatio
				sumLoad := 0.0
				for _, peer := range candidates {
					if _, ok := balancer.sche.regionPendings[peer.regionID]; ok {
						continue
					}

					splitRatio := balancer.sche.balanceRatio / peer.loads[maxID]
					if splitRatio >= 1 {
						continue
					}

					ops, infls := balancer.buildSplitOperation(peer, maxID, splitRatio)
					for i := 0; i < len(ops); i++ {
						// TODO: multiple operators need to be atomic.
						deci := &decision {
							srcStoreID: store.id,
							dstStoreID: store.id,
						}
						if !balancer.sche.addPendingInfluence(ops[i], deci, infls[i]) {
							return nil
						}
					}
					retOps = append(retOps, ops...)

					log.Info("create split operation",
						zap.Uint64("regionID", peer.regionID),
						zap.Uint64("storeID", store.id),
						zap.String("regionLoad", fmt.Sprintf("%+v", peer.loads)),
						zap.String("storeLoad", fmt.Sprintf("%+v", store.loads)),
						zap.String("splitRatio", fmt.Sprintf("%+v", splitRatio)),
						zap.Uint64("splitWhichLoad", maxID),
					)

					balancer.sche.hasSplit = true
				
					sumLoad += peer.loads[maxID]
					if sumLoad >= loadThreshold {
						break
					}
				}
			}
		}
	}

	return retOps
}

func (balancer *multiBalancer) getRegion(regionID uint64) *core.RegionInfo {
	region := balancer.cluster.GetRegion(regionID)

	if region == nil {
		schedulerCounter.WithLabelValues(balancer.sche.GetName(), "no-region").Inc()
		return nil
	}

	if !opt.IsHealthyAllowPending(balancer.cluster, region) {
		schedulerCounter.WithLabelValues(balancer.sche.GetName(), "unhealthy-replica").Inc()
		return nil
	}

	if !opt.IsRegionReplicated(balancer.cluster, region) {
		log.Debug("region has abnormal replica count", zap.String("scheduler", balancer.sche.GetName()), zap.Uint64("region-id", region.GetID()))
		schedulerCounter.WithLabelValues(balancer.sche.GetName(), "abnormal-replica").Inc()
		return nil
	}

	return region
}

func (balancer *multiBalancer) buildOperators() ([]*operator.Operator, []loadInfluence) {
	var (
		op       *operator.Operator
		counters []prometheus.Counter
		err      error
	)

	switch balancer.cur.opTy {
	case movePeer:
		srcPeer := balancer.cur.region.GetStorePeer(balancer.cur.srcStoreID) // checked in getRegionAndSrcPeer
		if srcPeer == nil {
			return nil, nil
		}
		dstPeer := &metapb.Peer{StoreId: balancer.cur.dstStoreID, Role: srcPeer.Role}
		desc := "move-hot-mix-peer"
		if balancer.cur.region.GetLeader() == srcPeer {
			op, err = operator.CreateMoveLeaderOperator(
				desc,
				balancer.cluster,
				balancer.cur.region,
				operator.OpHotRegion,
				balancer.cur.srcStoreID,
				dstPeer)
		} else {
			op, err = operator.CreateMovePeerOperator(
				desc,
				balancer.cluster,
				balancer.cur.region,
				operator.OpHotRegion,
				balancer.cur.srcStoreID,
				dstPeer)
		}

		counters = append(counters,
			hotDirectionCounter.WithLabelValues("move-peer", "mix", strconv.FormatUint(balancer.cur.srcStoreID, 10), "out"),
			hotDirectionCounter.WithLabelValues("move-peer", "mix", strconv.FormatUint(dstPeer.GetStoreId(), 10), "in"))
	case transferLeader:
		if balancer.cur.region.GetStoreVoter(balancer.cur.dstStoreID) == nil {
			return nil, nil
		}
		desc := "transfer-hot-mix-leader"
		op, err = operator.CreateTransferLeaderOperator(
			desc,
			balancer.cluster,
			balancer.cur.region,
			balancer.cur.srcStoreID,
			balancer.cur.dstStoreID,
			operator.OpHotRegion)
		counters = append(counters,
			hotDirectionCounter.WithLabelValues("transfer-leader", "mix", strconv.FormatUint(balancer.cur.srcStoreID, 10), "out"),
			hotDirectionCounter.WithLabelValues("transfer-leader", "mix", strconv.FormatUint(balancer.cur.dstStoreID, 10), "in"))
	}

	if err != nil {
		log.Info("fail to create operator", zap.String("rwType", "mix"), zap.Stringer("opType", balancer.cur.opTy), errs.ZapError(err))
		schedulerCounter.WithLabelValues(balancer.sche.GetName(), "create-operator-fail").Inc()
		return nil, nil
	}

	op.SetPriorityLevel(core.HighPriority)
	op.Counters = append(op.Counters, counters...)
	op.Counters = append(op.Counters,
		schedulerCounter.WithLabelValues(balancer.sche.GetName(), "new-operator"),
		schedulerCounter.WithLabelValues(balancer.sche.GetName(), balancer.cur.opTy.String()))

	loads := balancer.cur.srcPeerStat.GetLoads()
	if balancer.cur.opTy == transferLeader {
		loads[0] = 0
		loads[1] = 0
		loads[2] = 0
	}

	infl := loadInfluence{}
	for i := range infl.loads {
		infl.loads[i] = loads[i]
	}

	return []*operator.Operator{op}, []loadInfluence{infl}
}

func convertToSplitInfo(dimID uint64) (splitDim, splitType uint64) {
	splitType = 1 - uint64(dimID / 3) 	// for splitting: read 0, write 1
	switch dimID % 3 {
	case 0:
		splitDim = 0
	case 1, 2:
		splitDim = 1
	}
	return
}

func (balancer *multiBalancer) buildSplitOperation(pi *peerInfo, dimID uint64, splitRatio float64) ([]*operator.Operator, []loadInfluence) {
	splitDim, splitType := convertToSplitInfo(dimID)
	opts := []float64{float64(splitDim), splitRatio, float64(splitType)}
	region := balancer.cluster.GetRegion(pi.regionID)
	op := operator.CreateSplitRegionOperator("hotspot-split-region", region, operator.OpAdmin, pdpb.CheckPolicy_RATIO, nil, opts)
	op.SetPriorityLevel(core.HighPriority)
	
	infl := loadInfluence{}

	return []*operator.Operator{op}, []loadInfluence{infl}
}

func (h *multiDimensionScheduler) GetHotReadStatus() *statistics.StoreHotPeersInfos {
	return h.hotSched.GetHotReadStatus()
}

func (h *multiDimensionScheduler) GetHotWriteStatus() *statistics.StoreHotPeersInfos {
	return h.hotSched.GetHotWriteStatus()
}

func (h *multiDimensionScheduler) GetWritePendingInfluence() map[uint64]Influence {
	return h.hotSched.GetWritePendingInfluence()
}

func (h *multiDimensionScheduler) GetReadPendingInfluence() map[uint64]Influence {
	return h.hotSched.GetReadPendingInfluence()
}
