// Package webrtc contains WebRTC utilities.
package webrtc

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

const (
	// Initial bandwidth estimate (5 Mbps - start higher to allow quality selection)
	initialBandwidth = 5_000_000
	// Minimum bandwidth estimate (100 kbps)
	minBandwidth = 100_000
	// Maximum bandwidth estimate (50 Mbps)
	maxBandwidth = 50_000_000
	// EWMA smoothing factor for bandwidth estimation (lower = more responsive)
	bweSmoothingFactor = 0.7
	// Interval for bandwidth estimation updates
	bweUpdateInterval = 100 * time.Millisecond
)

// BandwidthEstimate contains the current bandwidth estimation.
type BandwidthEstimate struct {
	// Bitrate is the estimated available bitrate in bits per second
	Bitrate int
	// PacketLoss is the estimated packet loss ratio (0.0 - 1.0)
	PacketLoss float64
	// RTT is the estimated round-trip time
	RTT time.Duration
}

// BWECallback is called when bandwidth estimate changes significantly
type BWECallback func(estimate BandwidthEstimate)

// bweInterceptor implements bandwidth estimation based on TWCC feedback
type bweInterceptor struct {
	mu sync.RWMutex

	// Bandwidth estimation state
	estimatedBitrate int64
	packetLoss       float64
	rtt              time.Duration

	// Tracking sent packets for RTT calculation
	sentPackets    map[uint16]time.Time
	sentPacketsMu  sync.Mutex
	lastSeqNum     uint16
	totalSent      uint64
	totalLost      uint64
	totalAcked     uint64

	// Callback for bandwidth changes
	callback BWECallback

	// Tracking bytes sent for rate estimation
	bytesSent      uint64
	lastUpdateTime time.Time

	// Close channel
	closed chan struct{}
}

func newBWEInterceptor(callback BWECallback) *bweInterceptor {
	b := &bweInterceptor{
		estimatedBitrate: initialBandwidth,
		sentPackets:      make(map[uint16]time.Time),
		callback:         callback,
		lastUpdateTime:   time.Now(),
		closed:           make(chan struct{}),
	}

	// Start periodic bandwidth estimation
	go b.runEstimator()

	return b
}

func (b *bweInterceptor) runEstimator() {
	ticker := time.NewTicker(bweUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			b.updateEstimate()
		case <-b.closed:
			return
		}
	}
}

func (b *bweInterceptor) updateEstimate() {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(b.lastUpdateTime).Seconds()
	if elapsed <= 0 {
		return
	}

	// Calculate sending bitrate
	bytesSent := atomic.LoadUint64(&b.bytesSent)
	sendingBitrate := float64(bytesSent*8) / elapsed

	// Reset bytes counter
	atomic.StoreUint64(&b.bytesSent, 0)
	b.lastUpdateTime = now

	// Calculate packet loss ratio
	totalSent := atomic.LoadUint64(&b.totalSent)
	totalLost := atomic.LoadUint64(&b.totalLost)
	if totalSent > 0 {
		b.packetLoss = float64(totalLost) / float64(totalSent)
	}

	// Adjust bandwidth estimate based on packet loss
	// Using a simple congestion control algorithm
	var targetBitrate float64
	if b.packetLoss < 0.02 {
		// Very low loss - increase bandwidth
		targetBitrate = float64(b.estimatedBitrate) * 1.05
	} else if b.packetLoss < 0.10 {
		// Moderate loss - maintain
		targetBitrate = float64(b.estimatedBitrate)
	} else {
		// High loss - decrease bandwidth
		targetBitrate = float64(b.estimatedBitrate) * (1.0 - b.packetLoss)
	}

	// Also consider the actual sending rate
	if sendingBitrate > 0 && sendingBitrate < targetBitrate {
		// We're not sending as fast as estimated, don't increase beyond sending rate
		targetBitrate = min(targetBitrate, sendingBitrate*1.5)
	}

	// Apply smoothing
	newEstimate := bweSmoothingFactor*float64(b.estimatedBitrate) +
		(1-bweSmoothingFactor)*targetBitrate

	// Clamp to valid range
	newEstimate = max(minBandwidth, min(maxBandwidth, newEstimate))

	oldEstimate := b.estimatedBitrate
	b.estimatedBitrate = int64(newEstimate)

	// Notify callback if estimate changed significantly (>10%)
	if b.callback != nil {
		change := abs(float64(b.estimatedBitrate-oldEstimate)) / float64(oldEstimate)
		if change > 0.10 {
			go b.callback(BandwidthEstimate{
				Bitrate:    int(b.estimatedBitrate),
				PacketLoss: b.packetLoss,
				RTT:        b.rtt,
			})
		}
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// GetEstimate returns the current bandwidth estimate
func (b *bweInterceptor) GetEstimate() BandwidthEstimate {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return BandwidthEstimate{
		Bitrate:    int(b.estimatedBitrate),
		PacketLoss: b.packetLoss,
		RTT:        b.rtt,
	}
}

// Close closes the interceptor
func (b *bweInterceptor) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

// BindRTCPReader intercepts incoming RTCP packets (including TWCC feedback)
func (b *bweInterceptor) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(bytes []byte, attributes interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, attrs, err := reader.Read(bytes, attributes)
		if err != nil {
			return n, attrs, err
		}

		// Parse RTCP packets
		pkts, parseErr := rtcp.Unmarshal(bytes[:n])
		if parseErr != nil {
			return n, attrs, err
		}

		for _, pkt := range pkts {
			b.processRTCPPacket(pkt)
		}

		return n, attrs, err
	})
}

func (b *bweInterceptor) processRTCPPacket(pkt rtcp.Packet) {
	switch p := pkt.(type) {
	case *rtcp.TransportLayerCC:
		b.processTWCC(p)
	case *rtcp.ReceiverReport:
		b.processReceiverReport(p)
	case *rtcp.ReceiverEstimatedMaximumBitrate:
		b.processREMB(p)
	}
}

func (b *bweInterceptor) processTWCC(twcc *rtcp.TransportLayerCC) {
	b.sentPacketsMu.Lock()
	defer b.sentPacketsMu.Unlock()

	now := time.Now()

	// Process packet status chunks
	seqNum := twcc.BaseSequenceNumber
	for _, chunk := range twcc.PacketChunks {
		switch c := chunk.(type) {
		case *rtcp.RunLengthChunk:
			for i := uint16(0); i < c.RunLength; i++ {
				if sentTime, ok := b.sentPackets[seqNum]; ok {
					if c.PacketStatusSymbol == rtcp.TypeTCCPacketReceivedSmallDelta ||
						c.PacketStatusSymbol == rtcp.TypeTCCPacketReceivedLargeDelta {
						// Packet received - calculate RTT
						rtt := now.Sub(sentTime)
						b.mu.Lock()
						// EWMA for RTT
						b.rtt = time.Duration(0.8*float64(b.rtt) + 0.2*float64(rtt))
						b.mu.Unlock()
						atomic.AddUint64(&b.totalAcked, 1)
					} else if c.PacketStatusSymbol == rtcp.TypeTCCPacketNotReceived {
						atomic.AddUint64(&b.totalLost, 1)
					}
					delete(b.sentPackets, seqNum)
				}
				seqNum++
			}
		case *rtcp.StatusVectorChunk:
			for _, status := range c.SymbolList {
				if sentTime, ok := b.sentPackets[seqNum]; ok {
					if status == rtcp.TypeTCCPacketReceivedSmallDelta ||
						status == rtcp.TypeTCCPacketReceivedLargeDelta {
						rtt := now.Sub(sentTime)
						b.mu.Lock()
						b.rtt = time.Duration(0.8*float64(b.rtt) + 0.2*float64(rtt))
						b.mu.Unlock()
						atomic.AddUint64(&b.totalAcked, 1)
					} else if status == rtcp.TypeTCCPacketNotReceived {
						atomic.AddUint64(&b.totalLost, 1)
					}
					delete(b.sentPackets, seqNum)
				}
				seqNum++
			}
		}
	}
}

func (b *bweInterceptor) processReceiverReport(rr *rtcp.ReceiverReport) {
	for _, report := range rr.Reports {
		// Use fraction lost from receiver report
		fractionLost := float64(report.FractionLost) / 256.0

		b.mu.Lock()
		// EWMA for packet loss
		b.packetLoss = 0.8*b.packetLoss + 0.2*fractionLost
		b.mu.Unlock()
	}
}

func (b *bweInterceptor) processREMB(remb *rtcp.ReceiverEstimatedMaximumBitrate) {
	// REMB provides direct bandwidth estimate from receiver
	if remb.Bitrate > 0 {
		b.mu.Lock()
		// Use REMB as upper bound for our estimate
		if float64(remb.Bitrate) < float64(b.estimatedBitrate) {
			b.estimatedBitrate = int64(float64(remb.Bitrate) * 0.95) // Add some margin
		}
		b.mu.Unlock()
	}
}

// BindRTCPWriter intercepts outgoing RTCP packets
func (b *bweInterceptor) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return writer
}

// BindLocalStream intercepts outgoing RTP packets to track sequence numbers
func (b *bweInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		// Track sent packet
		b.sentPacketsMu.Lock()
		b.sentPackets[header.SequenceNumber] = time.Now()
		b.lastSeqNum = header.SequenceNumber
		// Cleanup old entries (keep last 1000)
		if len(b.sentPackets) > 1000 {
			// Find oldest entries to remove
			for k := range b.sentPackets {
				if len(b.sentPackets) <= 500 {
					break
				}
				delete(b.sentPackets, k)
			}
		}
		b.sentPacketsMu.Unlock()

		atomic.AddUint64(&b.totalSent, 1)
		atomic.AddUint64(&b.bytesSent, uint64(len(payload)+12)) // payload + RTP header

		return writer.Write(header, payload, attributes)
	})
}

// UnbindLocalStream is called when a stream is removed
func (b *bweInterceptor) UnbindLocalStream(info *interceptor.StreamInfo) {}

// BindRemoteStream binds to remote streams
func (b *bweInterceptor) BindRemoteStream(info *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	return reader
}

// UnbindRemoteStream is called when a remote stream is removed
func (b *bweInterceptor) UnbindRemoteStream(info *interceptor.StreamInfo) {}

// bweInterceptorFactory creates bweInterceptor instances
type bweInterceptorFactory struct {
	callback BWECallback
	onCreate func(b *bweInterceptor)
}

// NewInterceptor creates a new interceptor
func (f *bweInterceptorFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	b := newBWEInterceptor(f.callback)
	if f.onCreate != nil {
		f.onCreate(b)
	}
	return b, nil
}

