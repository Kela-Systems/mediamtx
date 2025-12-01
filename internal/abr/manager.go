package abr

import (
	"context"
	"fmt"
	"sync"

	"github.com/bluenviron/gortsplib/v5/pkg/description"

	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/logger"
	"github.com/bluenviron/mediamtx/internal/stream"
)

// PathManager is the interface for accessing paths.
type PathManager interface {
	AddReader(req defs.PathAddReaderReq) (defs.Path, *stream.Stream, error)
}

// Manager is the main ABR manager that tracks quality groups and provides
// adaptive reading capabilities.
type Manager struct {
	Log         logger.Writer
	PathManager PathManager

	mu           sync.RWMutex
	groupManager *GroupManager
	pathStreams  map[string]*pathStreamInfo // pathName -> stream info
	pathRefs     map[string]defs.Path       // pathName -> path reference
	ctx          context.Context
	ctxCancel    context.CancelFunc
}

type pathStreamInfo struct {
	stream *stream.Stream
	desc   *description.Session
	refs   int
}

// NewManager creates a new ABR manager.
func NewManager(log logger.Writer, pm PathManager) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		Log:          log,
		PathManager:  pm,
		groupManager: NewGroupManager(log),
		pathStreams:  make(map[string]*pathStreamInfo),
		pathRefs:     make(map[string]defs.Path),
		ctx:          ctx,
		ctxCancel:    cancel,
	}

	// Set up callbacks for group lifecycle
	m.groupManager.SetCallbacks(
		m.onGroupCreated,
		m.onGroupRemoved,
		m.onLevelChanged,
	)

	return m
}

// Close closes the manager.
func (m *Manager) Close() {
	m.ctxCancel()
}

// RegisterPath registers a path that has become ready.
// This should be called when a path's stream becomes available.
func (m *Manager) RegisterPath(pathName string, strm *stream.Stream, desc *description.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Store stream info
	m.pathStreams[pathName] = &pathStreamInfo{
		stream: strm,
		desc:   desc,
		refs:   0,
	}

	// Register with group manager
	m.groupManager.RegisterPath(pathName)
}

// UnregisterPath unregisters a path that is no longer ready.
func (m *Manager) UnregisterPath(pathName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.pathStreams, pathName)
	m.groupManager.UnregisterPath(pathName)
}

// SetPathAvailable updates the availability of a path.
func (m *Manager) SetPathAvailable(pathName string, available bool) {
	m.groupManager.SetPathAvailable(pathName, available)
}

// IsABRPath checks if the given path name is an ABR virtual path.
func (m *Manager) IsABRPath(pathName string) bool {
	return m.groupManager.IsABRPath(pathName)
}

// GetQualityGroup returns the quality group for a path prefix.
func (m *Manager) GetQualityGroup(prefix string) (*QualityGroup, bool) {
	return m.groupManager.GetGroup(prefix)
}

// GetGroupForPath returns the quality group that contains the given path.
func (m *Manager) GetGroupForPath(pathName string) (*QualityGroup, bool) {
	return m.groupManager.GetGroupForPath(pathName)
}

// GetPrefixForPath returns the ABR prefix for a path.
func (m *Manager) GetPrefixForPath(pathName string) string {
	return m.groupManager.GetPrefixForPath(pathName)
}

// GetStream returns the stream for a given path name.
// This implements the StreamProvider interface for AdaptiveReader.
func (m *Manager) GetStream(pathName string) (*stream.Stream, *description.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	info, ok := m.pathStreams[pathName]
	if !ok {
		// Try to get the stream from path manager
		// This will create the path if needed
		path, strm, err := m.PathManager.AddReader(defs.PathAddReaderReq{
			AccessRequest: defs.PathAccessRequest{
				Name:     pathName,
				SkipAuth: true, // Already authenticated at the ABR level
			},
		})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get stream for %s: %w", pathName, err)
		}

		// Get description from stream
		desc := strm.Desc

		info = &pathStreamInfo{
			stream: strm,
			desc:   desc,
			refs:   0,
		}
		m.pathStreams[pathName] = info
		m.pathRefs[pathName] = path

		// Also register with the group manager for ABR tracking
		m.groupManager.RegisterPath(pathName)
	}

	info.refs++
	return info.stream, info.desc, nil
}

// ReleaseStream releases a stream reference.
// This implements the StreamProvider interface for AdaptiveReader.
func (m *Manager) ReleaseStream(pathName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	info, ok := m.pathStreams[pathName]
	if !ok {
		return
	}

	info.refs--
	// Note: We don't remove the stream info here as the path may still be active
	// The stream will be cleaned up when UnregisterPath is called
}

// CreateAdaptiveReader creates a new adaptive reader for an ABR path.
func (m *Manager) CreateAdaptiveReader(
	prefix string,
	bandwidthProvider BandwidthProvider,
	parent logger.Writer,
	setupReader ReaderSetupFunc,
	prepareForSwitch PrepareForSwitchFunc,
) (*AdaptiveReader, error) {
	group, ok := m.groupManager.GetGroup(prefix)
	if !ok || !group.HasMultipleLevels() {
		return nil, fmt.Errorf("no active ABR group for prefix: %s", prefix)
	}

	return NewAdaptiveReader(group, m, bandwidthProvider, m.Log, parent, setupReader, prepareForSwitch), nil
}

// DiscoverQualityGroup attempts to discover quality levels for a prefix by
// probing common bitrate suffixes. This is used for lazy discovery of ABR groups.
func (m *Manager) DiscoverQualityGroup(prefix string) (*QualityGroup, error) {
	// First check if group already exists
	if group, ok := m.groupManager.GetGroup(prefix); ok && group.HasMultipleLevels() {
		return group, nil
	}

	// Common bitrates to probe (in kbps)
	commonBitrates := []int{
		8000, 6000, 5000, 4000, 3000, 2500, 2000,
		1500, 1200, 1000, 800, 600, 500, 400, 300, 200, 100,
	}

	discoveredPaths := make([]string, 0)

	// Try to discover paths with common bitrate suffixes
	for _, bitrate := range commonBitrates {
		pathName := BuildPathName(prefix, bitrate)

		// Try to get the stream - this will register it if successful
		_, _, err := m.GetStream(pathName)
		if err == nil {
			discoveredPaths = append(discoveredPaths, pathName)
			m.Log.Log(logger.Debug, "ABR: discovered quality level %s", pathName)
		}
	}

	if len(discoveredPaths) < 2 {
		// Clean up any discovered paths since we don't have enough for ABR
		for _, pathName := range discoveredPaths {
			m.ReleaseStream(pathName)
		}
		return nil, fmt.Errorf("not enough quality levels found for ABR prefix: %s (found %d, need at least 2)",
			prefix, len(discoveredPaths))
	}

	group, ok := m.groupManager.GetGroup(prefix)
	if !ok {
		return nil, fmt.Errorf("failed to create quality group for prefix: %s", prefix)
	}

	m.Log.Log(logger.Info, "ABR: discovered quality group %s with %d levels", prefix, len(discoveredPaths))
	return group, nil
}

// GetActiveGroups returns all active quality groups.
func (m *Manager) GetActiveGroups() map[string]*QualityGroup {
	return m.groupManager.GetActiveGroups()
}

// GetAllGroups returns all quality groups (including those with only one level).
func (m *Manager) GetAllGroups() map[string]*QualityGroup {
	return m.groupManager.GetAllGroups()
}

// Callbacks from GroupManager

func (m *Manager) onGroupCreated(prefix string) {
	m.Log.Log(logger.Info, "ABR group created: %s", prefix)
}

func (m *Manager) onGroupRemoved(prefix string) {
	m.Log.Log(logger.Info, "ABR group removed: %s", prefix)
}

func (m *Manager) onLevelChanged(prefix string, levels QualityLevels) {
	m.Log.Log(logger.Debug, "ABR group %s levels changed: %d levels", prefix, len(levels))
}

// BandwidthProviderFunc is a function that implements BandwidthProvider.
type BandwidthProviderFunc func() int

// GetBandwidth implements BandwidthProvider.
func (f BandwidthProviderFunc) GetBandwidth() int {
	return f()
}

