// SPDX-FileCopyrightText: 2021 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package packet

import (
	"context"
	"fmt"
	"m/metrics"
	"net"
	"strings"
	"sync"
	"time"
	"unsafe"

	"encoding/binary"
	"github.com/cilium/ebpf"
	"k8s.io/klog/v2"

	"m/promextra"
)

// #include "./c/types.h"
import "C"

type NetworkDataSource struct {
	cidrs      map[string]struct{}
	ports      map[string]struct{}
	ebpfConfig *ebpfConfig
	attachment *ebpfAttachment
}

type State struct {
	snis map[string]time.Time
}

type ConnKey struct {
	sourceIP, destIP string
	sni string
}

// NewNetworkDataSource creates a new network data source based on
// eBPF that loads the socket filtering program on the given network
// interface and sets the program according to the given CIDRs and
// ports.
func NewNetworkDataSource(networkInterface string, cidrs, ports map[string]struct{}) (*NetworkDataSource, error) {
	ec, err := newEBPFConfig()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			ec.Close()
		}
	}()

	if err := initCIDRMap(ec.cidrMap, cidrs); err != nil {
		return nil, fmt.Errorf("initializing CIDR map: %w", err)
	}
	if err := initPortMap(ec.portMap, ports); err != nil {
		return nil, fmt.Errorf("initializing port map: %w", err)
	}
	if err := initStatsMap(ec.statsMap); err != nil {
		return nil, fmt.Errorf("initializing stats map: %w", err)
	}

	attachment, err := attachProgramToNetworkInterface(ec.prog, networkInterface)
	if err != nil {
		return nil, err
	}

	s := &NetworkDataSource{
		cidrs:      cidrs,
		ports:      ports,
		ebpfConfig: ec,
		attachment: attachment,
	}

	return s, nil
}

// Close cleans up the network data source.
func (s *NetworkDataSource) Close() error {
	if s.attachment != nil {
		s.attachment.Close()
		s.attachment = nil
	}
	if s.ebpfConfig != nil {
		s.ebpfConfig.Close()
		s.ebpfConfig = nil
	}
	return nil
}

// TrackExecutionTime periodically reads the histogram snapshots from
// the eBPF map and sends them over the channel.
func (s *NetworkDataSource) TrackExecutionTime(ctx context.Context, wg *sync.WaitGroup, ticks <-chan time.Time, snapshots chan<- promextra.Snapshot) {
	defer wg.Done()
	defer close(snapshots)
	done := ctx.Done()
	for {
		select {
		case <-ticks:
			snapshots <- s.readHistogramSnapshot()
		case <-done:
			return
		}
	}
}

func (s *NetworkDataSource) readHistogramSnapshot() promextra.Snapshot {
	snapshot, err := readSnapshotFromMap(s.ebpfConfig.histogramMap)
	if err != nil {
		klog.Fatalf("failed to read histogram snapshot from eBPF map: %v", err)
	}
	return snapshot
}

// AsSet splits the provided comma-separated string and returns a map where
// the key is a substring and the value is dummy.
func AsSet(list string) map[string]struct{} {
	r := map[string]struct{}{}
	for _, item := range strings.Split(list, ",") {
		r[item] = struct{}{}
	}
	return r
}

// TrackConnections tracks the connections in connections map which are older than 20 seconds
// and updates the prometheus inc counters as per data received from the map.
// It also tracks information from stats map and retrieves value of succecced and failed seconds.
// Those values are updated as prometheus counters.
func (s *NetworkDataSource) TrackConnections(ctx context.Context, wg *sync.WaitGroup, ticks <-chan time.Time, incs chan<- *metrics.Inc) {
	defer wg.Done()
	state := newState()
	var key C.struct_tuple_key_t
	var val C.struct_tuple_data_t
	var currentTickerClock uint64

	// keep track of failed second between ticks for each SNI in order to
	// carry over failed seconds during inactive seconds.
	previousFailedSecond := map[ConnKey]bool{}

	done := ctx.Done()
	for {
		select {
		case <-ticks:
			// Set of encountered SNIs, in either of the 2 maps
			sniSet := map[ConnKey]struct{}{}
			// oldConnections are the connections that were initiated C.STATS_SECONDS_COUNT seconds ago
			oldConnections := make(map[C.struct_tuple_key_t]*tupleData)
			connections := s.ebpfConfig.connectionMap.Iterate()
			for connections.Next(unsafe.Pointer(&key), unsafe.Pointer(&val)) {
				data := tupleDataFromC(val)

				// Entry will be only added if the connection is old.
				if isConnectionOld(data.tickerClockFirstPacket, currentTickerClock) {
					oldConnections[key] = data
				}
			}

			if err := connections.Err(); err != nil {
				klog.Errorf("reading connections from map: %v", err)
				continue
			}

			for k, t := range oldConnections {
				if t.sni == "" {
					klog.Errorf("Empty SNI\nDATA: %+v\n%+v", k, t)
				}
				// Delete old connections.
				// We do not want to check error while deleting
				_ = s.ebpfConfig.connectionMap.Delete(unsafe.Pointer(&k))
			}

			statsKey := (currentTickerClock + 1) % 20
			statsValuesAtKey, err := getOldestStatsAndCleanup(s, statsKey)
			if err != nil {
				klog.Errorf("getting stats from map: %v", err)
				continue
			}

			// Get the union of SNIs from both BPF maps. Some SNIs
			// might be in connectionMap only, in statsMap only, or
			// in both.
			for k, v := range oldConnections {
				sourceIP := net.IP{}
				binary.LittleEndian.PutUint32(sourceIP, uint32(k.source_ip))
				destIP := net.IP{}
				binary.LittleEndian.PutUint32(destIP, uint32(k.dest_ip))
				sniSet[ConnKey{sourceIP: sourceIP.String(), destIP: destIP.String(), sni: v.sni}] = struct{}{}
			}
			for k := range statsValuesAtKey {
				sniSet[k] = struct{}{}
			}

			staleConnections := make(map[ConnKey][]*tupleData)
			for sni := range sniSet {
				staleConnections[sni] = []*tupleData{}
			}

			for k, v := range oldConnections {
				sourceIP := net.IP{}
				binary.LittleEndian.PutUint32(sourceIP, uint32(k.source_ip))
				destIP := net.IP{}
				binary.LittleEndian.PutUint32(destIP, uint32(k.dest_ip))
				ck := ConnKey{sourceIP: sourceIP.String(), destIP: destIP.String(), sni: v.sni}
				staleConnections[ck] = append(staleConnections[ck], v)
			}

			for sni := range sniSet {
				var succeeded_connections, failed_connections uint64
				if completedConnections, ok := statsValuesAtKey[sni]; ok {
					succeeded_connections = completedConnections[0]
					failed_connections = completedConnections[1]
				}

				if _, ok := previousFailedSecond[sni]; !ok {
					previousFailedSecond[sni] = false
				}
				inc, failedSecond := state.accountForConnections(sni, previousFailedSecond[sni], staleConnections[sni], succeeded_connections, failed_connections)
				previousFailedSecond[sni] = failedSecond
				incs <- inc
			}

			state.deleteExpiredSNIs(time.Now())

			// Update the counter to new value.
			currentTickerClock++
			if err := s.ebpfConfig.tickerClockMap.Put(uint32(0), currentTickerClock); err != nil {
				klog.Errorf("updating tickerClockMap: %v", err)
				continue
			}
		case <-done:
			return
		}
	}
}

func (s *State) deleteExpiredSNIs(now time.Time) {
	for name, lastUpdate := range s.snis {
		// Expire metrics if the last update is older than the metric expiration
		if lastUpdate.Add(metrics.Expiration).Before(now) {
			delete(s.snis, name)
			metrics.DeleteMetrics(name)
		}
	}
}

func newState() *State {
	return &State{
		snis: make(map[string]time.Time),
	}
}

// getOldestStatsAndCleanup reads the stats map at the given index and cleans up the inner stats map at that index.
// Returned variable out is a map of sni to:
// - succeeded_connections := innerValue[0]
// - failed_connections := innerValue[1]
func getOldestStatsAndCleanup(s *NetworkDataSource, statsKey uint64) (out map[ConnKey][2]uint64, err error) {
	var innerMap *ebpf.Map

	if err := s.ebpfConfig.statsMap.Lookup(unsafe.Pointer(&statsKey), &innerMap); err != nil {
		return nil, err
	}
	var innerKey string
	var innerValue [2]uint64
	var innerKeysToBeDeleted []string
	out = make(map[ConnKey][2]uint64)
	innerEntries := innerMap.Iterate()
	for innerEntries.Next(&innerKey, &innerValue) {
		key := ConnKey{
			sourceIP: net.IP(innerKey[0:4]).String(),
			destIP: net.IP(innerKey[4:8]).String(),
			sni: strings.SplitN(innerKey[8:], "\000", 2)[0],
		}
		klog.InfoS("getOldestStatsAndCleanup", "source", key.sourceIP, "dest", key.destIP, "sni", key.sni)
		out[key] = innerValue
		innerKeysToBeDeleted = append(innerKeysToBeDeleted, innerKey)
	}

	for _, v := range innerKeysToBeDeleted {
		// We do not want to check error while deleting
		_ = innerMap.Delete(v)
	}

	if err := innerEntries.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// isConnectionOld checks whether the connection is older than 20 seconds.
func isConnectionOld(tickerClockFirstPacket, current_ticker_clock uint64) bool {
	return current_ticker_clock > C.STATS_SECONDS_COUNT+uint64(tickerClockFirstPacket)
}

func (s *State) accountForConnections(
	connKey ConnKey,
	previousFailedSecond bool,
	staleConnMapInfo []*tupleData,
	succeeded_connections, failed_connections uint64,
) (i *metrics.Inc, failedSecond bool) {
	if connKey.sourceIP == "" {
		klog.Error("source IP is empty")
	}
	if _, ok := s.snis[connKey.sni]; !ok {
		s.snis[connKey.sni] = time.Now()
	}
	inc := &metrics.Inc{SNI: connKey.sni, SourceIP: connKey.sourceIP, DestIP: connKey.destIP}

	klog.V(2).Infof("sni: %s, connections: %d", connKey.sni, len(staleConnMapInfo))
	var activeSecond, activeFailedSecond bool

	for _, v := range staleConnMapInfo {
		state := v.state
		// TODO handle all the states
		// note: TCP FIN state is ambiguous, rejection depends on who sent the RST packet
		if state == SYN_RECEIVED || state == SYNACK_RECEIVED {
			activeFailedSecond = true
		}

		if state == SNI_RECEIVED {
			inc.SuccessfulConnections++
		}

		if state == RST_SENT_BY_SERVER {
			activeFailedSecond = true
			inc.RejectedConnections++
		}

		if state == RST_SENT_BY_CLIENT {
			inc.RejectedConnectionsByClient++
		}
	}

	inc.SuccessfulConnections += float64(succeeded_connections)
	inc.RejectedConnections += float64(failed_connections)

	if len(staleConnMapInfo) > 0 || succeeded_connections > 0 || failed_connections > 0 {
		activeSecond = true
	}
	if failed_connections > 0 {
		activeFailedSecond = true
	}

	// the second failed if we carry the failure from before, or if there is a new failure
	if (previousFailedSecond && !activeSecond) || activeFailedSecond {
		failedSecond = true
	}

	if activeFailedSecond {
		inc.ActiveFailedSeconds++
	}

	if activeSecond {
		inc.ActiveSeconds++
	}

	if failedSecond {
		inc.FailedSeconds++
	}

	return inc, failedSecond
}
