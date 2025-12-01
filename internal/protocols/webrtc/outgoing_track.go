package webrtc

import (
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/rtpsender"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// OutgoingTrack is a WebRTC outgoing track
type OutgoingTrack struct {
	Caps webrtc.RTPCodecCapability

	track      *webrtc.TrackLocalStaticRTP
	ssrc       uint32
	rtcpSender *rtpsender.Sender

	// Timestamp continuity for ABR switching
	tsMu              sync.Mutex
	lastTimestamp     uint32 // Last timestamp sent to browser
	timestampOffset   uint32 // Offset to add to incoming timestamps
	firstTsReceived   bool   // Whether we've received the first timestamp
	firstTsValue      uint32 // First timestamp value from current source
	switchPending     bool   // Whether a stream switch is pending
}

func (t *OutgoingTrack) isVideo() bool {
	return strings.Split(t.Caps.MimeType, "/")[0] == "video"
}

func (t *OutgoingTrack) setup(p *PeerConnection) error {
	var trackID string
	if t.isVideo() {
		trackID = "video"
	} else {
		trackID = "audio"
	}

	var err error
	t.track, err = webrtc.NewTrackLocalStaticRTP(
		t.Caps,
		trackID,
		webrtcStreamID,
	)
	if err != nil {
		return err
	}

	sender, err := p.wr.AddTrack(t.track)
	if err != nil {
		return err
	}

	t.ssrc = uint32(sender.GetParameters().Encodings[0].SSRC)

	t.rtcpSender = &rtpsender.Sender{
		ClockRate: int(t.track.Codec().ClockRate),
		Period:    1 * time.Second,
		TimeNow:   time.Now,
		WritePacketRTCP: func(pkt rtcp.Packet) {
			p.wr.WriteRTCP([]rtcp.Packet{pkt}) //nolint:errcheck
		},
	}
	t.rtcpSender.Initialize()

	// incoming RTCP packets must always be read to make interceptors work
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err2 := sender.Read(buf)
			if err2 != nil {
				return
			}

			_, err2 = rtcp.Unmarshal(buf[:n])
			if err2 != nil {
				panic(err2)
			}
		}
	}()

	return nil
}

func (t *OutgoingTrack) close() {
	if t.rtcpSender != nil {
		t.rtcpSender.Close()
	}
}

// WriteRTP writes a RTP packet.
func (t *OutgoingTrack) WriteRTP(pkt *rtp.Packet) error {
	return t.WriteRTPWithNTP(pkt, time.Now())
}

// WriteRTPWithNTP writes a RTP packet.
func (t *OutgoingTrack) WriteRTPWithNTP(pkt *rtp.Packet, ntp time.Time) error {
	// use right SSRC in packet to make rtcpSender work
	pkt.SSRC = t.ssrc

	// Handle timestamp continuity for ABR
	t.tsMu.Lock()
	if t.switchPending && t.lastTimestamp > 0 {
		// Stream switch with existing data - calculate offset for continuity
		if !t.firstTsReceived {
			t.firstTsReceived = true
			t.firstTsValue = pkt.Timestamp
			// Calculate offset: we want new timestamp to continue from last + small gap
			// Add a small gap (1 frame at 30fps = 3000 at 90kHz)
			targetTimestamp := t.lastTimestamp + 3000
			if targetTimestamp > t.firstTsValue {
				t.timestampOffset = targetTimestamp - t.firstTsValue
			} else {
				// New stream has higher timestamps - no offset needed, just reset
				t.timestampOffset = 0
			}
			t.switchPending = false
		}
	} else if t.switchPending {
		// First time setup (no previous data) - no offset needed
		t.switchPending = false
		t.timestampOffset = 0
	}

	// Apply timestamp offset
	if t.timestampOffset != 0 {
		pkt.Timestamp += t.timestampOffset
	}
	t.lastTimestamp = pkt.Timestamp
	t.tsMu.Unlock()

	t.rtcpSender.ProcessPacket(pkt, ntp, true)

	return t.track.WriteRTP(pkt)
}

// PrepareForSwitch marks the track as preparing for a stream switch.
// Call this before switching to a new quality level.
func (t *OutgoingTrack) PrepareForSwitch() {
	t.tsMu.Lock()
	defer t.tsMu.Unlock()
	t.switchPending = true
	t.firstTsReceived = false
}
