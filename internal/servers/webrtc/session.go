package webrtc

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/google/uuid"
	"github.com/pion/ice/v4"
	"github.com/pion/sdp/v3"
	pwebrtc "github.com/pion/webrtc/v4"

	"github.com/bluenviron/mediamtx/internal/abr"
	"github.com/bluenviron/mediamtx/internal/auth"
	"github.com/bluenviron/mediamtx/internal/conf"
	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/externalcmd"
	"github.com/bluenviron/mediamtx/internal/hooks"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/protocols/httpp"
	"github.com/bluenviron/mediamtx/internal/protocols/webrtc"
	"github.com/bluenviron/mediamtx/internal/stream"
)

func whipOffer(body []byte) *pwebrtc.SessionDescription {
	return &pwebrtc.SessionDescription{
		Type: pwebrtc.SDPTypeOffer,
		SDP:  string(body),
	}
}

type sessionParent interface {
	closeSession(sx *session)
	generateICEServers(clientConfig bool) ([]pwebrtc.ICEServer, error)
	logger.Writer
}

type session struct {
	udpReadBufferSize     uint
	parentCtx             context.Context
	ipsFromInterfaces     bool
	ipsFromInterfacesList []string
	additionalHosts       []string
	iceUDPMux             ice.UDPMux
	iceTCPMux             *webrtc.TCPMuxWrapper
	handshakeTimeout      conf.Duration
	trackGatherTimeout    conf.Duration
	stunGatherTimeout     conf.Duration
	req                   webRTCNewSessionReq
	wg                    *sync.WaitGroup
	externalCmdPool       *externalcmd.Pool
	pathManager           serverPathManager
	abrManager            *abr.Manager
	parent                sessionParent

	ctx       context.Context
	ctxCancel func()
	created   time.Time
	uuid      uuid.UUID
	secret    uuid.UUID
	mutex     sync.RWMutex
	pc        *webrtc.PeerConnection

	chNew           chan webRTCNewSessionReq
	chAddCandidates chan webRTCAddSessionCandidatesReq
}

func (s *session) initialize() {
	ctx, ctxCancel := context.WithCancel(s.parentCtx)

	s.ctx = ctx
	s.ctxCancel = ctxCancel
	s.created = time.Now()
	s.uuid = uuid.New()
	s.secret = uuid.New()
	s.chNew = make(chan webRTCNewSessionReq)
	s.chAddCandidates = make(chan webRTCAddSessionCandidatesReq)

	s.Log(logger.Info, "created by %s", s.req.remoteAddr)

	s.wg.Add(1)

	go s.run()
}

// Log implements logger.Writer.
func (s *session) Log(level logger.Level, format string, args ...any) {
	id := hex.EncodeToString(s.uuid[:4])
	s.parent.Log(level, "[session %v] "+format, append([]any{id}, args...)...)
}

func (s *session) Close() {
	s.ctxCancel()
}

func (s *session) run() {
	defer s.wg.Done()

	err := s.runInner()

	s.ctxCancel()

	s.parent.closeSession(s)

	s.Log(logger.Info, "closed: %v", err)
}

func (s *session) runInner() error {
	select {
	case <-s.chNew:
	case <-s.ctx.Done():
		return fmt.Errorf("terminated")
	}

	errStatusCode, err := s.runInner2()

	if errStatusCode != 0 {
		s.req.res <- webRTCNewSessionRes{
			errStatusCode: errStatusCode,
			err:           err,
		}
	}

	return err
}

func (s *session) runInner2() (int, error) {
	if s.req.publish {
		return s.runPublish()
	}
	return s.runRead()
}

func (s *session) runPublish() (int, error) {
	ip, _, _ := net.SplitHostPort(s.req.remoteAddr)

	pathConf, err := s.pathManager.FindPathConf(defs.PathFindPathConfReq{
		AccessRequest: defs.PathAccessRequest{
			Name:        s.req.pathName,
			Query:       s.req.httpRequest.URL.RawQuery,
			Publish:     true,
			Proto:       auth.ProtocolWebRTC,
			ID:          &s.uuid,
			Credentials: httpp.Credentials(s.req.httpRequest),
			IP:          net.ParseIP(ip),
		},
	})
	if err != nil {
		return http.StatusBadRequest, err
	}

	iceServers, err := s.parent.generateICEServers(false)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	pc := &webrtc.PeerConnection{
		UDPReadBufferSize:     s.udpReadBufferSize,
		ICEUDPMux:             s.iceUDPMux,
		ICETCPMux:             s.iceTCPMux,
		ICEServers:            iceServers,
		IPsFromInterfaces:     s.ipsFromInterfaces,
		IPsFromInterfacesList: s.ipsFromInterfacesList,
		AdditionalHosts:       s.additionalHosts,
		HandshakeTimeout:      s.handshakeTimeout,
		TrackGatherTimeout:    s.trackGatherTimeout,
		STUNGatherTimeout:     s.stunGatherTimeout,
		Publish:               false,
		Log:                   s,
	}
	err = pc.Start()
	if err != nil {
		return http.StatusBadRequest, err
	}

	terminatorDone := make(chan struct{})
	defer func() { <-terminatorDone }()

	terminatorRun := make(chan struct{})
	defer close(terminatorRun)

	go func() {
		defer close(terminatorDone)
		select {
		case <-s.ctx.Done():
		case <-terminatorRun:
		}
		pc.Close()
	}()

	offer := whipOffer(s.req.offer)

	var sdp sdp.SessionDescription
	err = sdp.Unmarshal([]byte(offer.SDP))
	if err != nil {
		return http.StatusBadRequest, err
	}

	err = webrtc.TracksAreValid(sdp.MediaDescriptions)
	if err != nil {
		// RFC draft-ietf-wish-whip
		// if the number of audio and or video
		// tracks or number streams is not supported by the WHIP Endpoint, it
		// MUST reject the HTTP POST request with a "406 Not Acceptable" error
		// response.
		return http.StatusNotAcceptable, err
	}

	answer, err := pc.CreateFullAnswer(offer)
	if err != nil {
		return http.StatusBadRequest, err
	}

	s.writeAnswer(answer)

	go s.readRemoteCandidates(pc)

	err = pc.WaitUntilConnected()
	if err != nil {
		return 0, err
	}

	s.mutex.Lock()
	s.pc = pc
	s.mutex.Unlock()

	err = pc.GatherIncomingTracks()
	if err != nil {
		return 0, err
	}

	var stream *stream.Stream

	medias, err := webrtc.ToStream(pc, pathConf, &stream, s)
	if err != nil {
		return 0, err
	}

	var path defs.Path
	path, stream, err = s.pathManager.AddPublisher(defs.PathAddPublisherReq{
		Author:             s,
		Desc:               &description.Session{Medias: medias},
		GenerateRTPPackets: false,
		FillNTP:            !pathConf.UseAbsoluteTimestamp,
		ConfToCompare:      pathConf,
		AccessRequest: defs.PathAccessRequest{
			Name:     s.req.pathName,
			Query:    s.req.httpRequest.URL.RawQuery,
			Publish:  true,
			SkipAuth: true,
		},
	})
	if err != nil {
		return 0, err
	}

	defer path.RemovePublisher(defs.PathRemovePublisherReq{Author: s})

	pc.StartReading()

	select {
	case <-pc.Failed():
		return 0, fmt.Errorf("peer connection closed")

	case <-s.ctx.Done():
		return 0, fmt.Errorf("terminated")
	}
}

func (s *session) runRead() (int, error) {
	ip, _, _ := net.SplitHostPort(s.req.remoteAddr)

	req := defs.PathAccessRequest{
		Name:        s.req.pathName,
		Query:       s.req.httpRequest.URL.RawQuery,
		Proto:       auth.ProtocolWebRTC,
		ID:          &s.uuid,
		Credentials: httpp.Credentials(s.req.httpRequest),
		IP:          net.ParseIP(ip),
	}

	// Check if this path should use ABR
	// ABR is only used when requesting the virtual path (prefix), not specific quality level paths
	var abrPrefix string
	if s.abrManager != nil {
		// Check if the path itself is an ABR virtual path (the prefix)
		if s.abrManager.IsABRPath(s.req.pathName) {
			abrPrefix = s.req.pathName
		} else {
			// Check if this could be an ABR prefix by trying to discover quality levels
			// This handles the case where user requests "mystream" and we have "mystream_2000", "mystream_1000"
			// But NOT when user requests "mystream_2000" directly - that should use standard reading
			_, bitrate, isQualityLevel := abr.ParsePathBitrate(s.req.pathName)
			if !isQualityLevel || bitrate == 0 {
				// Not a quality level path - try to discover ABR group
				_, err := s.abrManager.DiscoverQualityGroup(s.req.pathName)
				if err == nil {
					abrPrefix = s.req.pathName
					s.Log(logger.Info, "discovered ABR group for prefix: %s", abrPrefix)
				}
			}
			// If it IS a quality level path (like mystream_2000), use standard reading
		}
	}

	// If ABR is available for this path, use adaptive reading
	if abrPrefix != "" {
		return s.runReadABR(req, abrPrefix)
	}

	// Standard reading (non-ABR)
	return s.runReadStandard(req)
}

func (s *session) runReadStandard(req defs.PathAccessRequest) (int, error) {
	path, strm, err := s.pathManager.AddReader(defs.PathAddReaderReq{
		Author:        s,
		AccessRequest: req,
	})
	if err != nil {
		var terr2 defs.PathNoStreamAvailableError
		if errors.As(err, &terr2) {
			return http.StatusNotFound, err
		}

		return http.StatusBadRequest, err
	}

	defer path.RemoveReader(defs.PathRemoveReaderReq{Author: s})

	iceServers, err := s.parent.generateICEServers(false)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	pc := &webrtc.PeerConnection{
		UDPReadBufferSize:     s.udpReadBufferSize,
		ICEUDPMux:             s.iceUDPMux,
		ICETCPMux:             s.iceTCPMux,
		ICEServers:            iceServers,
		IPsFromInterfaces:     s.ipsFromInterfaces,
		IPsFromInterfacesList: s.ipsFromInterfacesList,
		AdditionalHosts:       s.additionalHosts,
		HandshakeTimeout:      s.handshakeTimeout,
		TrackGatherTimeout:    s.trackGatherTimeout,
		STUNGatherTimeout:     s.stunGatherTimeout,
		Publish:               true,
		Log:                   s,
	}

	r := &stream.Reader{Parent: s}

	err = webrtc.FromStream(strm.Desc, r, pc)
	if err != nil {
		return http.StatusBadRequest, err
	}

	err = pc.Start()
	if err != nil {
		return http.StatusBadRequest, err
	}

	terminatorDone := make(chan struct{})
	defer func() { <-terminatorDone }()

	terminatorRun := make(chan struct{})
	defer close(terminatorRun)

	go func() {
		defer close(terminatorDone)
		select {
		case <-s.ctx.Done():
		case <-terminatorRun:
		}
		pc.Close()
	}()

	offer := whipOffer(s.req.offer)

	answer, err := pc.CreateFullAnswer(offer)
	if err != nil {
		return http.StatusBadRequest, err
	}

	s.writeAnswer(answer)

	go s.readRemoteCandidates(pc)

	err = pc.WaitUntilConnected()
	if err != nil {
		return 0, err
	}

	s.mutex.Lock()
	s.pc = pc
	s.mutex.Unlock()

	s.Log(logger.Info, "is reading from path '%s', %s",
		path.Name(), defs.FormatsInfo(r.Formats()))

	onUnreadHook := hooks.OnRead(hooks.OnReadParams{
		Logger:          s,
		ExternalCmdPool: s.externalCmdPool,
		Conf:            path.SafeConf(),
		ExternalCmdEnv:  path.ExternalCmdEnv(),
		Reader:          s.APIReaderDescribe(),
		Query:           s.req.httpRequest.URL.RawQuery,
	})
	defer onUnreadHook()

	strm.AddReader(r)
	defer strm.RemoveReader(r)

	select {
	case <-pc.Failed():
		return 0, fmt.Errorf("peer connection closed")

	case err = <-r.Error():
		return 0, err

	case <-s.ctx.Done():
		return 0, fmt.Errorf("terminated")
	}
}

func (s *session) runReadABR(accessReq defs.PathAccessRequest, abrPrefix string) (int, error) {
	// Get the quality group (should already be discovered at this point)
	group, ok := s.abrManager.GetQualityGroup(abrPrefix)
	if !ok || !group.HasMultipleLevels() {
		// Fall back to standard reading if group is not valid
		s.Log(logger.Debug, "ABR group not valid for %s, falling back to standard reading", abrPrefix)
		return s.runReadStandard(accessReq)
	}

	iceServers, err := s.parent.generateICEServers(false)
	if err != nil {
		return http.StatusInternalServerError, err
	}

	// Create peer connection
	pc := &webrtc.PeerConnection{
		UDPReadBufferSize:     s.udpReadBufferSize,
		ICEUDPMux:             s.iceUDPMux,
		ICETCPMux:             s.iceTCPMux,
		ICEServers:            iceServers,
		IPsFromInterfaces:     s.ipsFromInterfaces,
		IPsFromInterfacesList: s.ipsFromInterfacesList,
		AdditionalHosts:       s.additionalHosts,
		HandshakeTimeout:      s.handshakeTimeout,
		TrackGatherTimeout:    s.trackGatherTimeout,
		STUNGatherTimeout:     s.stunGatherTimeout,
		Publish:               true,
		Log:                   s,
	}

	// Get any quality level to set up initial tracks (all levels should have same codec)
	anyLevel, ok := group.LowestLevel()
	if !ok {
		return http.StatusNotFound, fmt.Errorf("no quality levels available for: %s", abrPrefix)
	}

	// Get stream to set up tracks
	_, initialDesc, err := s.abrManager.GetStream(anyLevel.Path)
	if err != nil {
		return http.StatusNotFound, err
	}

	// Set up tracks using a dummy reader - we just need the tracks created
	dummyReader := &stream.Reader{Parent: s}
	err = webrtc.FromStream(initialDesc, dummyReader, pc)
	if err != nil {
		s.abrManager.ReleaseStream(anyLevel.Path)
		return http.StatusBadRequest, err
	}
	s.abrManager.ReleaseStream(anyLevel.Path)

	err = pc.Start()
	if err != nil {
		return http.StatusBadRequest, err
	}

	terminatorDone := make(chan struct{})
	defer func() { <-terminatorDone }()

	terminatorRun := make(chan struct{})
	defer close(terminatorRun)

	// Create bandwidth provider
	bandwidthProvider := abr.BandwidthProviderFunc(func() int {
		return pc.GetBandwidthEstimate().Bitrate
	})

	// Create setup function that uses FromStream for each quality level
	setupReader := func(desc *description.Session, r *stream.Reader) error {
		// We need to set up encoding callbacks that write to the existing tracks.
		// FromStream would create new tracks, so we use a custom setup.
		return webrtc.SetupReaderCallbacks(desc, r, pc)
	}

	// Create prepare for switch function
	prepareForSwitch := func() {
		pc.PrepareTracksForSwitch()
	}

	adaptiveReader := abr.NewAdaptiveReader(
		group,
		s.abrManager,
		bandwidthProvider,
		s,
		s, // parent for stream.Reader
		setupReader,
		prepareForSwitch,
	)

	go func() {
		defer close(terminatorDone)
		select {
		case <-s.ctx.Done():
		case <-terminatorRun:
		}
		adaptiveReader.Close()
		pc.Close()
	}()

	offer := whipOffer(s.req.offer)

	answer, err := pc.CreateFullAnswer(offer)
	if err != nil {
		return http.StatusBadRequest, err
	}

	s.writeAnswer(answer)

	go s.readRemoteCandidates(pc)

	err = pc.WaitUntilConnected()
	if err != nil {
		return 0, err
	}

	s.mutex.Lock()
	s.pc = pc
	s.mutex.Unlock()

	// Start adaptive reader
	err = adaptiveReader.Start()
	if err != nil {
		return 0, err
	}

	s.Log(logger.Info, "is reading from ABR path '%s' (starting level: %s)",
		abrPrefix, adaptiveReader.CurrentPath())

	select {
	case <-pc.Failed():
		return 0, fmt.Errorf("peer connection closed")

	case <-s.ctx.Done():
		return 0, fmt.Errorf("terminated")
	}
}

func (s *session) writeAnswer(answer *pwebrtc.SessionDescription) {
	s.req.res <- webRTCNewSessionRes{
		sx:     s,
		answer: []byte(answer.SDP),
	}
}

func (s *session) readRemoteCandidates(pc *webrtc.PeerConnection) {
	for {
		select {
		case req := <-s.chAddCandidates:
			for _, candidate := range req.candidates {
				err := pc.AddRemoteCandidate(candidate)
				if err != nil {
					req.res <- webRTCAddSessionCandidatesRes{err: err}
				}
			}
			req.res <- webRTCAddSessionCandidatesRes{}

		case <-s.ctx.Done():
			return
		}
	}
}

// new is called by webRTCHTTPServer through Server.
func (s *session) new(req webRTCNewSessionReq) webRTCNewSessionRes {
	select {
	case s.chNew <- req:
		return <-req.res

	case <-s.ctx.Done():
		return webRTCNewSessionRes{err: fmt.Errorf("terminated"), errStatusCode: http.StatusInternalServerError}
	}
}

// addCandidates is called by webRTCHTTPServer through Server.
func (s *session) addCandidates(
	req webRTCAddSessionCandidatesReq,
) webRTCAddSessionCandidatesRes {
	select {
	case s.chAddCandidates <- req:
		return <-req.res

	case <-s.ctx.Done():
		return webRTCAddSessionCandidatesRes{err: fmt.Errorf("terminated")}
	}
}

// APIReaderDescribe implements reader.
func (s *session) APIReaderDescribe() defs.APIPathSourceOrReader {
	return defs.APIPathSourceOrReader{
		Type: "webRTCSession",
		ID:   s.uuid.String(),
	}
}

// APISourceDescribe implements source.
func (s *session) APISourceDescribe() defs.APIPathSourceOrReader {
	return s.APIReaderDescribe()
}

func (s *session) apiItem() *defs.APIWebRTCSession {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	peerConnectionEstablished := false
	localCandidate := ""
	remoteCandidate := ""
	bytesReceived := uint64(0)
	bytesSent := uint64(0)
	rtpPacketsReceived := uint64(0)
	rtpPacketsSent := uint64(0)
	rtpPacketsLost := uint64(0)
	rtpPacketsJitter := float64(0)
	rtcpPacketsReceived := uint64(0)
	rtcpPacketsSent := uint64(0)

	if s.pc != nil {
		peerConnectionEstablished = true
		localCandidate = s.pc.LocalCandidate()
		remoteCandidate = s.pc.RemoteCandidate()
		stats := s.pc.Stats()
		bytesReceived = stats.BytesReceived
		bytesSent = stats.BytesSent
		rtpPacketsReceived = stats.RTPPacketsReceived
		rtpPacketsSent = stats.RTPPacketsSent
		rtpPacketsLost = stats.RTPPacketsLost
		rtpPacketsJitter = stats.RTPPacketsJitter
		rtcpPacketsReceived = stats.RTCPPacketsReceived
		rtcpPacketsSent = stats.RTCPPacketsSent
	}

	return &defs.APIWebRTCSession{
		ID:                        s.uuid,
		Created:                   s.created,
		RemoteAddr:                s.req.remoteAddr,
		PeerConnectionEstablished: peerConnectionEstablished,
		LocalCandidate:            localCandidate,
		RemoteCandidate:           remoteCandidate,
		State: func() defs.APIWebRTCSessionState {
			if s.req.publish {
				return defs.APIWebRTCSessionStatePublish
			}
			return defs.APIWebRTCSessionStateRead
		}(),
		Path:                s.req.pathName,
		Query:               s.req.httpRequest.URL.RawQuery,
		BytesReceived:       bytesReceived,
		BytesSent:           bytesSent,
		RTPPacketsReceived:  rtpPacketsReceived,
		RTPPacketsSent:      rtpPacketsSent,
		RTPPacketsLost:      rtpPacketsLost,
		RTPPacketsJitter:    rtpPacketsJitter,
		RTCPPacketsReceived: rtcpPacketsReceived,
		RTCPPacketsSent:     rtcpPacketsSent,
	}
}
