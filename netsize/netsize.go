package netsize

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p-core/peer"
	kbucket "github.com/libp2p/go-libp2p-kbucket"
	ks "github.com/whyrusleeping/go-keyspace"
)

var (
	ErrNotEnoughData      = fmt.Errorf("not enough data")
	ErrWrongNumOfPeers    = fmt.Errorf("expected bucket size number of peers")
	ErrUncertaintyTooHigh = fmt.Errorf("estimate uncertainty too high") // TODO: unused
)

var (
	logger                   = logging.Logger("dht/netsize")
	MaxMeasurementAge        = 2 * time.Hour
	MinMeasurementsThreshold = 5
	MaxMeasurementsThreshold = 150
	keyspaceMaxInt, _        = new(big.Int).SetString(strings.Repeat("F", 64), 16)
	keyspaceMaxFloat         = new(big.Float).SetInt(keyspaceMaxInt)
)

type Estimator struct {
	localID    kbucket.ID
	rt         *kbucket.RoutingTable
	bucketSize int

	measurementsLk sync.RWMutex
	measurements   map[int][]measurement

	netSizeCache *float64
}

func NewEstimator(localID peer.ID, rt *kbucket.RoutingTable, bucketSize int) *Estimator {
	// initialize map to hold measurement observations
	measurements := map[int][]measurement{}
	for i := 0; i < bucketSize; i++ {
		measurements[i] = []measurement{}
	}

	return &Estimator{
		localID:      kbucket.ConvertPeerID(localID),
		rt:           rt,
		bucketSize:   bucketSize,
		measurements: measurements,
	}
}

type measurement struct {
	distance  float64
	weight    float64
	timestamp time.Time
}

// Track tracks the list of peers for the given key to incorporate in the next network size estimate.
// key is expected **NOT** to be in the kademlia keyspace and peers is expected to be a sorted list of
// the closest peers to the given key (the closest first).
// This function expects peers to have the same length as the routing table bucket size. It also
// strips old and limits the number of data points (favouring new).
func (e *Estimator) Track(key string, peers []peer.ID) error {
	e.measurementsLk.Lock()
	defer e.measurementsLk.Unlock()

	// sanity check
	if len(peers) != e.bucketSize {
		return ErrWrongNumOfPeers
	}

	logger.Debugw("Tracking peers for key", "key", key)

	now := time.Now()

	// invalidate cache
	e.netSizeCache = nil

	// Calculate weight for the peer distances.
	weight := e.calcWeight(key)

	// Map given key to the Kademlia key space (hash it)
	ksKey := ks.XORKeySpace.Key([]byte(key))

	// the maximum age timestamp of the measurement data points
	maxAgeTs := now.Add(-MaxMeasurementAge)

	for i, p := range peers {
		// Map peer to the kademlia key space
		pKey := ks.XORKeySpace.Key([]byte(p))

		// Construct measurement struct
		m := measurement{
			distance:  NormedDistance(pKey, ksKey),
			weight:    weight,
			timestamp: now,
		}

		// keep track of this measurement
		e.measurements[i] = append(e.measurements[i], m)

		// find the smallest index of a measurement that is still in the allowed time window
		// all measurements with a lower index should be discarded as they are too old
		n := len(e.measurements[i])
		idx := sort.Search(n, func(j int) bool {
			return e.measurements[i][j].timestamp.After(maxAgeTs)
		})

		// if measurements are outside the allowed time window remove them.
		// idx == n - there is no measurement in the allowed time window -> reset slice
		// idx == 0 - the normal case where we only have valid entries
		// idx != 0 - there is a mix of valid and obsolete entries
		if idx == n {
			e.measurements[i] = []measurement{}
		} else if idx != 0 {
			e.measurements[i] = e.measurements[i][idx:]
		}

		// if the number of data points exceed the max threshold, strip oldest measurement data points.
		if len(e.measurements[i]) > MaxMeasurementsThreshold {
			e.measurements[i] = e.measurements[i][len(e.measurements[i])-MaxMeasurementsThreshold:]
		}
	}

	return nil
}

// NetworkSize instructs the Estimator to calculate the current network size estimate.
func (e *Estimator) NetworkSize() (float64, error) {
	e.measurementsLk.Lock()
	defer e.measurementsLk.Unlock()

	// return cached calculation
	if e.netSizeCache != nil {
		logger.Debugw("Cached network size estimation", "estimate", *e.netSizeCache)
		return *e.netSizeCache, nil
	}

	// remove obsolete data points
	e.garbageCollect()

	// initialize slices for linear fit
	xs := make([]float64, e.bucketSize)
	ys := make([]float64, e.bucketSize)
	yerrs := make([]float64, e.bucketSize)

	for i := 0; i < e.bucketSize; i++ {
		observationCount := len(e.measurements[i])

		// If we don't have enough data to reasonably calculate the network size, return early
		if observationCount < MinMeasurementsThreshold {
			return 0, ErrNotEnoughData
		}

		// Calculate Average Distance
		sumDistances := float64(0)
		sumWeights := float64(0)
		for _, m := range e.measurements[i] {
			sumDistances += m.weight * m.distance
			sumWeights += m.weight
		}
		distanceAvg := sumDistances / sumWeights

		// Calculate standard deviation
		sumWeightedDiffs := float64(0)
		for _, m := range e.measurements[i] {
			diff := m.distance - distanceAvg
			sumWeightedDiffs += m.weight * diff * diff
		}
		variance := sumWeightedDiffs / (float64((observationCount - 1)) / float64(observationCount) * sumWeights)
		distanceStd := math.Sqrt(variance)

		// Track calculations
		xs[i] = float64(i + 1)
		ys[i] = distanceAvg
		yerrs[i] = distanceStd
	}

	// Calculate linear regression (assumes the line goes through the origin)
	var x2Sum, xySum float64
	for i, xi := range xs {
		yi := ys[i]
		xySum += yerrs[i] * xi * yi
		x2Sum += yerrs[i] * xi * xi
	}
	slope := xySum / x2Sum

	// cache network size estimation
	netSize := 1/slope - 1
	e.netSizeCache = &netSize

	logger.Debugw("New network size estimation", "estimate", *e.netSizeCache)
	return netSize, nil
}

// calcWeight weighs data points exponentially less if they fall into a non-full bucket.
// It weighs distance estimates based on their CPLs and bucket levels.
// Bucket Level: 20 -> 1/2^0 -> weight: 1
// Bucket Level: 17 -> 1/2^3 -> weight: 1/8
// Bucket Level: 10 -> 1/2^10 -> weight: 1/1024
func (e *Estimator) calcWeight(key string) float64 {
	cpl := kbucket.CommonPrefixLen(kbucket.ConvertKey(key), e.localID)
	bucketLevel := e.rt.NPeersForCpl(uint(cpl))
	return math.Pow(2, float64(bucketLevel-e.bucketSize))
}

// NormedDistance calculates the normed XOR distance of the given keys (from 0 to 1).
func NormedDistance(key1 ks.Key, key2 ks.Key) float64 {
	ksDistance := new(big.Float).SetInt(key1.Distance(key2))
	normedDist, _ := new(big.Float).Quo(ksDistance, keyspaceMaxFloat).Float64()
	return normedDist
}

// garbageCollect removes all measurements from the list that fell out of the measurement time window.
func (e *Estimator) garbageCollect() {
	logger.Debug("Running garbage collection")
	now := time.Now()

	// the maximum age timestamp of the measurement data points
	maxAgeTs := now.Add(-MaxMeasurementAge)

	for i := 0; i < e.bucketSize; i++ {

		// find the smallest index of a measurement that is still in the allowed time window
		// all measurements with a lower index should be discarded as they are too old
		n := len(e.measurements[i])
		idx := sort.Search(n, func(j int) bool {
			return e.measurements[i][j].timestamp.After(maxAgeTs)
		})

		// if measurements are outside the allowed time window remove them.
		// idx == n - there is no measurement in the allowed time window -> reset slice
		// idx == 0 - the normal case where we only have valid entries
		// idx != 0 - there is a mix of valid and obsolete entries
		if idx == n {
			e.measurements[i] = []measurement{}
		} else if idx != 0 {
			e.measurements[i] = e.measurements[i][idx:]
		}
	}
}