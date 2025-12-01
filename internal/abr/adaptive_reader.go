package abr

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"

	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
)

const (
	// Minimum interval between quality switches to prevent oscillation
	minSwitchInterval = 3 * time.Second
	// Interval for bandwidth monitoring
	bandwidthCheckInterval = 500 * time.Millisecond
)

// StreamProvider provides access to streams by path name.
type StreamProvider interface {
	GetStream(pathName string) (*stream.Stream, *description.Session, error)
	ReleaseStream(pathName string)
}

// BandwidthProvider provides current bandwidth estimate.
type BandwidthProvider interface {
	GetBandwidth() int // Returns estimated bandwidth in bps
}

// ReaderSetupFunc is called to set up a reader for a specific stream description.
// It should register callbacks on the reader that encode and write to tracks.
type ReaderSetupFunc func(desc *description.Session, r *stream.Reader) error

// PrepareForSwitchFunc is called to prepare tracks for a stream switch.
// This should be called after removing the old reader and before adding the new one.
type PrepareForSwitchFunc func()

// AdaptiveReaderState represents the current state of the adaptive reader.
type AdaptiveReaderState struct {
	CurrentPath    string
	CurrentBitrate int
	Bandwidth      int
	LastSwitch     time.Time
}

// AdaptiveReader reads from a quality group and automatically switches
// between quality levels based on bandwidth estimation.
type AdaptiveReader struct {
	Group              *QualityGroup
	StreamProvider     StreamProvider
	BandwidthProvider  BandwidthProvider
	Log                logger.Writer
	Parent             logger.Writer // For stream.Reader
	SetupReader        ReaderSetupFunc
	PrepareForSwitch   PrepareForSwitchFunc

	mu sync.RWMutex

	// Current state
	currentPath   string
	currentStream *stream.Stream
	currentDesc   *description.Session
	reader        *stream.Reader

	// Switching state
	lastSwitchTime time.Time
	logCounter     int

	// Control
	ctx       chan struct{}
	closeOnce sync.Once
	closed    int32 // atomic
}

// NewAdaptiveReader creates a new AdaptiveReader.
func NewAdaptiveReader(
	group *QualityGroup,
	streamProvider StreamProvider,
	bandwidthProvider BandwidthProvider,
	log logger.Writer,
	parent logger.Writer,
	setupReader ReaderSetupFunc,
	prepareForSwitch PrepareForSwitchFunc,
) *AdaptiveReader {
	return &AdaptiveReader{
		Group:              group,
		StreamProvider:     streamProvider,
		BandwidthProvider:  bandwidthProvider,
		Log:                log,
		Parent:             parent,
		SetupReader:        setupReader,
		PrepareForSwitch:   prepareForSwitch,
		ctx:                make(chan struct{}),
	}
}

// Start starts the adaptive reader with an initial quality level.
func (ar *AdaptiveReader) Start() error {
	// Select initial quality level based on current bandwidth
	bandwidth := ar.BandwidthProvider.GetBandwidth()
	level, ok := ar.Group.SelectLevel(bandwidth, "")
	if !ok {
		// Fall back to lowest available level (safest for unknown bandwidth)
		level, ok = ar.Group.LowestLevel()
		if !ok {
			return &NoQualityLevelError{Prefix: ar.Group.Prefix}
		}
	}

	ar.mu.Lock()
	ar.lastSwitchTime = time.Now()
	ar.mu.Unlock()

	err := ar.switchToLevel(level.Path)
	if err != nil {
		return err
	}

	// Start bandwidth monitoring goroutine
	go ar.monitorBandwidth()

	return nil
}

// GetState returns the current state of the reader.
func (ar *AdaptiveReader) GetState() AdaptiveReaderState {
	ar.mu.RLock()
	defer ar.mu.RUnlock()

	bitrate := 0
	for _, level := range ar.Group.GetLevels() {
		if level.Path == ar.currentPath {
			bitrate = level.Bitrate
			break
		}
	}

	return AdaptiveReaderState{
		CurrentPath:    ar.currentPath,
		CurrentBitrate: bitrate,
		Bandwidth:      ar.BandwidthProvider.GetBandwidth(),
		LastSwitch:     ar.lastSwitchTime,
	}
}

// Close closes the adaptive reader and releases resources.
func (ar *AdaptiveReader) Close() {
	ar.closeOnce.Do(func() {
		atomic.StoreInt32(&ar.closed, 1)
		close(ar.ctx)

		ar.mu.Lock()
		if ar.currentStream != nil && ar.reader != nil {
			ar.currentStream.RemoveReader(ar.reader)
		}
		if ar.currentPath != "" {
			ar.StreamProvider.ReleaseStream(ar.currentPath)
		}
		ar.mu.Unlock()
	})
}

func (ar *AdaptiveReader) monitorBandwidth() {
	ticker := time.NewTicker(bandwidthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ar.checkBandwidthAndSwitch()
		case <-ar.ctx:
			return
		}
	}
}

func (ar *AdaptiveReader) checkBandwidthAndSwitch() {
	if atomic.LoadInt32(&ar.closed) == 1 {
		return
	}

	ar.mu.RLock()
	currentPath := ar.currentPath
	lastSwitch := ar.lastSwitchTime
	ar.mu.RUnlock()

	bandwidth := ar.BandwidthProvider.GetBandwidth()

	// Log bandwidth estimation periodically (every ~30 seconds = 60 checks)
	ar.logCounter++
	if ar.logCounter%60 == 0 {
		ar.Log.Log(logger.Info, "ABR: bandwidth estimate: %d kbps, current level: %s",
			bandwidth/1000, currentPath)
	}

	// Don't switch too frequently
	if time.Since(lastSwitch) < minSwitchInterval {
		return
	}

	newLevel, ok := ar.Group.SelectLevel(bandwidth, currentPath)
	if !ok {
		ar.Log.Log(logger.Debug, "ABR: no suitable level found for bandwidth %d kbps", bandwidth/1000)
		return
	}

	// Only switch if the level is different
	if newLevel.Path == currentPath {
		return
	}

	ar.Log.Log(logger.Info, "ABR: switching from %s to %s (bandwidth: %d kbps, target: %d kbps)",
		currentPath, newLevel.Path, bandwidth/1000, newLevel.Bitrate/1000)

	// Perform immediate switch
	err := ar.switchToLevel(newLevel.Path)
	if err != nil {
		ar.Log.Log(logger.Error, "ABR: failed to switch to %s: %v", newLevel.Path, err)
	}
}

func (ar *AdaptiveReader) switchToLevel(pathName string) error {
	// Get the new stream
	newStream, newDesc, err := ar.StreamProvider.GetStream(pathName)
	if err != nil {
		return err
	}

	// Create a new reader for this stream
	newReader := &stream.Reader{Parent: ar.Parent}

	// Set up encoding callbacks for this stream's media/format objects
	if ar.SetupReader != nil {
		err = ar.SetupReader(newDesc, newReader)
		if err != nil {
			ar.StreamProvider.ReleaseStream(pathName)
			return fmt.Errorf("failed to setup reader: %w", err)
		}
	}

	ar.mu.Lock()
	defer ar.mu.Unlock()

	// Remove old reader from old stream
	if ar.currentStream != nil && ar.reader != nil {
		ar.currentStream.RemoveReader(ar.reader)
	}
	if ar.currentPath != "" && ar.currentPath != pathName {
		ar.StreamProvider.ReleaseStream(ar.currentPath)
	}

	// Prepare tracks for switch AFTER removing old reader, BEFORE adding new one
	// This ensures no packets from old stream can interfere with offset calculation
	if ar.PrepareForSwitch != nil && ar.currentPath != "" {
		ar.PrepareForSwitch()
	}

	oldPath := ar.currentPath
	ar.currentPath = pathName
	ar.currentStream = newStream
	ar.currentDesc = newDesc
	ar.reader = newReader
	ar.lastSwitchTime = time.Now()

	ar.Log.Log(logger.Info, "ABR: adding reader to stream for path %s", pathName)

	// Add reader to new stream
	newStream.AddReader(ar.reader)

	if oldPath != "" {
		ar.Log.Log(logger.Info, "ABR: switched from %s to %s", oldPath, pathName)
	} else {
		ar.Log.Log(logger.Info, "ABR: started reading from %s", pathName)
	}

	return nil
}


// Reader returns the underlying stream reader.
// Note: This may return nil if Start() hasn't been called yet.
func (ar *AdaptiveReader) Reader() *stream.Reader {
	ar.mu.RLock()
	defer ar.mu.RUnlock()
	return ar.reader
}

// CurrentDesc returns the current stream description.
func (ar *AdaptiveReader) CurrentDesc() *description.Session {
	ar.mu.RLock()
	defer ar.mu.RUnlock()
	return ar.currentDesc
}

// CurrentPath returns the current quality level path.
func (ar *AdaptiveReader) CurrentPath() string {
	ar.mu.RLock()
	defer ar.mu.RUnlock()
	return ar.currentPath
}

// NoQualityLevelError is returned when no quality level is available.
type NoQualityLevelError struct {
	Prefix string
}

func (e *NoQualityLevelError) Error() string {
	return "no quality level available for " + e.Prefix
}

