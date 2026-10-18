package perf

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/ring"
)

var (
	ErrNotImplemented = errors.New("not implemented")
	ErrNoGpuTool      = errors.New("no GPU monitoring tool available")
)

type Monitor struct {
	mutex    sync.RWMutex
	log      *logmon.Monitor
	conf     config.PerformanceConfig
	power    config.PowerConfig
	sysRing  ring.Buffer[SysStat]
	gpuRing  ring.Buffer[[]GpuStat]
	nodeRing ring.Buffer[NodeStat]

	// energy accumulators integrate power samples into watt-hours since
	// process start. They survive UpdateConfig restarts so the Prometheus
	// counters stay monotonic for the lifetime of the process.
	gpuEnergy  map[string]*energyAccumulator
	nodeEnergy *energyAccumulator

	stopCtx    context.Context
	stopCancel context.CancelFunc

	sysListeners  map[chan SysStat]struct{}
	gpuListeners  map[chan []GpuStat]struct{}
	nodeListeners map[chan NodeStat]struct{}
}

func ringCapacity(c config.PerformanceConfig) int {
	n := int(time.Hour / c.Every)
	if n < 1 {
		n = 1
	}
	return n
}

func New(c config.PerformanceConfig, power config.PowerConfig, logger *logmon.Monitor) (*Monitor, error) {

	if c.Every < 100*time.Millisecond {
		c.Every = 100 * time.Millisecond
	}

	if logger == nil {
		return nil, errors.New("logger is required")
	}

	capacity := ringCapacity(c)
	return &Monitor{
		conf:          c,
		power:         power,
		log:           logger,
		sysRing:       ring.NewBuffer[SysStat](capacity),
		gpuRing:       ring.NewBuffer[[]GpuStat](capacity),
		nodeRing:      ring.NewBuffer[NodeStat](nodeRingCapacity()),
		gpuEnergy:     make(map[string]*energyAccumulator),
		nodeEnergy:    &energyAccumulator{name: "node"},
		sysListeners:  make(map[chan SysStat]struct{}),
		gpuListeners:  make(map[chan []GpuStat]struct{}),
		nodeListeners: make(map[chan NodeStat]struct{}),
	}, nil
}

// nodeRingCapacity keeps about one hour of node power samples at the fixed
// IPMI sampling cadence.
func nodeRingCapacity() int {
	n := int(time.Hour / nodePowerEvery)
	if n < 1 {
		n = 1
	}
	return n
}

func (m *Monitor) Stop() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if m.stopCancel == nil {
		return
	}
	m.stopCancel()
	m.stopCancel = nil
}

// UpdateConfig updates the monitor configuration and restarts if changed.
func (m *Monitor) UpdateConfig(newConf config.PerformanceConfig, newPower config.PowerConfig) {
	m.mutex.RLock()
	changed := m.conf != newConf || !reflect.DeepEqual(m.power, newPower)
	m.mutex.RUnlock()

	if !changed {
		return
	}

	m.Stop()
	m.mutex.Lock()
	m.conf = newConf
	m.power = newPower
	capacity := ringCapacity(newConf)
	m.sysRing = ring.NewBuffer[SysStat](capacity)
	m.gpuRing = ring.NewBuffer[[]GpuStat](capacity)
	m.nodeRing = ring.NewBuffer[NodeStat](nodeRingCapacity())
	m.mutex.Unlock()
	if !newConf.Disabled {
		m.Start()
	}
}

// Subscribe returns channels to listen to system, GPU and node power stats.
// GPU and node channels deliver whole batches; all are non-blocking sends so
// a slow subscriber drops samples instead of stalling the monitor.
func (m *Monitor) Subscribe() (chan SysStat, chan []GpuStat, chan NodeStat, func()) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	sysChan := make(chan SysStat, 1)
	gpuChan := make(chan []GpuStat, 1)
	nodeChan := make(chan NodeStat, 1)

	m.sysListeners[sysChan] = struct{}{}
	m.gpuListeners[gpuChan] = struct{}{}
	m.nodeListeners[nodeChan] = struct{}{}

	unsub := func() {
		m.mutex.Lock()
		defer m.mutex.Unlock()
		delete(m.sysListeners, sysChan)
		delete(m.gpuListeners, gpuChan)
		delete(m.nodeListeners, nodeChan)
	}

	return sysChan, gpuChan, nodeChan, unsub
}

func (m *Monitor) Start() {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	if m.stopCancel != nil {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.stopCtx, m.stopCancel = ctx, cancel
	// Snapshot the configuration while the mutex is held: the loops below must
	// not read m.conf/m.power after UpdateConfig starts rewriting them, and
	// they receive ctx as an argument so a Stop/Start swap can never leave
	// one of them parked on the new context.
	every := m.conf.Every
	ipmi := m.power.Ipmi

	go func(ctx context.Context) {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s, err := ReadSysStats()
				if err != nil {
					if err != ErrNotImplemented {
						m.log.Errorf("failed to read sys stats: %s", err.Error())
					}
					continue
				}
				m.mutex.Lock()
				m.sysRing.Push(s)
				for l := range m.sysListeners {
					select {
					case l <- s:
					default:
					}
				}
				m.mutex.Unlock()
			}
		}
	}(ctx)

	go func(ctx context.Context) {
		gpuCh, err := getGpuStats(ctx, every, m.log)
		if err != nil {
			if errors.Is(err, ErrNotImplemented) || errors.Is(err, ErrNoGpuTool) {
				m.log.Infof("GPU monitoring not available: %s", err.Error())
			} else {
				m.log.Errorf("failed to initialize GPU monitoring: %s", err.Error())
			}
			return
		}

		for {
			select {
			case <-ctx.Done():
				return
			case g, ok := <-gpuCh:
				if !ok {
					m.log.Errorf("failed reading from gpuCh - stopping read goroutine")
					return
				}
				m.mutex.Lock()
				m.gpuRing.Push(g)
				m.integrateGpuEnergyLocked(g)
				for l := range m.gpuListeners {
					select {
					case l <- g:
					default:
					}
				}
				m.mutex.Unlock()
			}
		}
	}(ctx)

	if ipmi.Disabled {
		return
	}

	go func(ctx context.Context) {
		nodeCh, err := collectNodePower(ctx, ipmi.Command, m.log)
		if err != nil {
			if errors.Is(err, ErrNoNodePowerTool) {
				m.log.Infof("node power monitoring not available: %s", err.Error())
			} else {
				m.log.Errorf("failed to initialize node power monitoring: %s", err.Error())
			}
			return
		}

		for {
			select {
			case <-ctx.Done():
				return
			case n, ok := <-nodeCh:
				if !ok {
					return
				}
				m.mutex.Lock()
				m.nodeRing.Push(n)
				// 0 W is a valid reading: the accumulator advances its anchor
				// so the next real sample does not bill the gap at the stale
				// wattage. Negative readings are discarded inside add().
				m.nodeEnergy.add(n.Timestamp, n.PowerW)
				for l := range m.nodeListeners {
					select {
					case l <- n:
					default:
					}
				}
				m.mutex.Unlock()
			}
		}
	}(ctx)
}

// integrateGpuEnergyLocked folds one batch of GPU samples into the energy
// accumulators; callers must hold m.mutex.
func (m *Monitor) integrateGpuEnergyLocked(stats []GpuStat) {
	for _, stat := range stats {
		key := gpuEnergyKey{id: stat.ID, uuid: stat.UUID}.String()
		acc := m.gpuEnergy[key]
		if acc == nil {
			acc = &energyAccumulator{id: stat.ID, name: stat.Name, uuid: stat.UUID}
			m.gpuEnergy[key] = acc
		}
		acc.add(stat.Timestamp, stat.PowerDrawW)
	}
}

// Current returns a copy of the current log of system, GPU and node stats.
func (m *Monitor) Current() ([]SysStat, []GpuStat, []NodeStat) {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	sysStats := m.sysRing.Slice()

	snapshots := m.gpuRing.Slice()
	var gpuStats []GpuStat
	for _, snapshot := range snapshots {
		gpuStats = append(gpuStats, snapshot...)
	}
	return sysStats, gpuStats, m.nodeRing.Slice()
}

// GpuEnergyTotal is the cumulative energy consumption of one GPU since
// process start.
type GpuEnergyTotal struct {
	ID        int
	Name      string
	UUID      string
	WattHours float64
}

// EnergySnapshot reports the integrated energy totals feeding the Prometheus
// counters.
type EnergySnapshot struct {
	GPUs         []GpuEnergyTotal
	Node         GpuEnergyTotal
	NodeHasValue bool
}

// Energy returns a snapshot of the energy accumulators.
func (m *Monitor) Energy() EnergySnapshot {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	snapshot := EnergySnapshot{Node: GpuEnergyTotal{Name: "node", WattHours: m.nodeEnergy.totalWh}}
	snapshot.NodeHasValue = m.nodeEnergy.lastW > 0

	ids := make([]string, 0, len(m.gpuEnergy))
	for key := range m.gpuEnergy {
		ids = append(ids, key)
	}
	sort.Strings(ids)
	for _, key := range ids {
		acc := m.gpuEnergy[key]
		snapshot.GPUs = append(snapshot.GPUs, GpuEnergyTotal{
			ID:        acc.id,
			Name:      acc.name,
			UUID:      acc.uuid,
			WattHours: acc.totalWh,
		})
	}
	return snapshot
}

// NodePowerAvailable reports whether node-level power readings have been seen.
func (m *Monitor) NodePowerAvailable() bool {
	m.mutex.RLock()
	defer m.mutex.RUnlock()
	return m.nodeEnergy.lastW > 0
}

func ReadSysStats() (SysStat, error) {
	return readSysStats()
}

func GetGpuStats(ctx context.Context, every time.Duration, logger *logmon.Monitor) (chan []GpuStat, error) {
	return getGpuStats(ctx, every, logger)
}
