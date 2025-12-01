package abr

import (
	"sync"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// GroupManager manages all quality groups and tracks path registrations.
type GroupManager struct {
	Log logger.Writer

	mu     sync.RWMutex
	groups map[string]*QualityGroup // prefix -> QualityGroup

	// Callbacks for virtual path management
	onGroupCreated  func(prefix string)
	onGroupRemoved  func(prefix string)
	onLevelChanged  func(prefix string, levels QualityLevels)
}

// NewGroupManager creates a new GroupManager.
func NewGroupManager(log logger.Writer) *GroupManager {
	return &GroupManager{
		Log:    log,
		groups: make(map[string]*QualityGroup),
	}
}

// SetCallbacks sets the callbacks for group lifecycle events.
func (gm *GroupManager) SetCallbacks(
	onGroupCreated func(prefix string),
	onGroupRemoved func(prefix string),
	onLevelChanged func(prefix string, levels QualityLevels),
) {
	gm.mu.Lock()
	defer gm.mu.Unlock()
	gm.onGroupCreated = onGroupCreated
	gm.onGroupRemoved = onGroupRemoved
	gm.onLevelChanged = onLevelChanged
}

// RegisterPath registers a path and potentially adds it to a quality group.
// Returns the prefix if the path belongs to a quality group, empty string otherwise.
func (gm *GroupManager) RegisterPath(pathName string) string {
	prefix, bitrate, ok := ParsePathBitrate(pathName)
	if !ok {
		return ""
	}

	gm.mu.Lock()

	group, exists := gm.groups[prefix]
	if !exists {
		group = NewQualityGroup(prefix)
		gm.groups[prefix] = group
	}

	group.AddLevel(pathName, bitrate)

	// Check if we need to notify about group creation
	shouldNotifyCreated := !exists && gm.onGroupCreated != nil
	shouldNotifyChanged := exists && gm.onLevelChanged != nil
	levels := group.GetLevels()

	// Also check if this is the second level being added (group becomes active)
	if exists && len(levels) == 2 && gm.onGroupCreated != nil {
		shouldNotifyCreated = true
		shouldNotifyChanged = false
	}

	gm.mu.Unlock()

	if shouldNotifyCreated {
		gm.Log.Log(logger.Info, "ABR: created quality group '%s' with levels: %v", prefix, levels)
		gm.onGroupCreated(prefix)
	} else if shouldNotifyChanged {
		gm.Log.Log(logger.Debug, "ABR: updated quality group '%s': %v", prefix, levels)
		gm.onLevelChanged(prefix, levels)
	}

	return prefix
}

// UnregisterPath removes a path from its quality group.
// Returns true if the path was part of a group.
func (gm *GroupManager) UnregisterPath(pathName string) bool {
	prefix, _, ok := ParsePathBitrate(pathName)
	if !ok {
		return false
	}

	gm.mu.Lock()

	group, exists := gm.groups[prefix]
	if !exists {
		gm.mu.Unlock()
		return false
	}

	group.RemoveLevel(pathName)

	shouldRemoveGroup := group.IsEmpty()
	shouldNotifyRemoved := shouldRemoveGroup && gm.onGroupRemoved != nil
	shouldNotifyChanged := !shouldRemoveGroup && gm.onLevelChanged != nil

	// Also check if we went from 2 levels to 1 (group becomes inactive)
	levels := group.GetLevels()
	if len(levels) == 1 && gm.onGroupRemoved != nil {
		shouldNotifyRemoved = true
		shouldNotifyChanged = false
	}

	if shouldRemoveGroup {
		delete(gm.groups, prefix)
	}

	gm.mu.Unlock()

	if shouldNotifyRemoved {
		gm.Log.Log(logger.Info, "ABR: removed quality group '%s'", prefix)
		gm.onGroupRemoved(prefix)
	} else if shouldNotifyChanged {
		gm.Log.Log(logger.Debug, "ABR: updated quality group '%s': %v", prefix, levels)
		gm.onLevelChanged(prefix, levels)
	}

	return true
}

// SetPathAvailable updates the availability of a path.
func (gm *GroupManager) SetPathAvailable(pathName string, available bool) {
	prefix, _, ok := ParsePathBitrate(pathName)
	if !ok {
		return
	}

	gm.mu.RLock()
	group, exists := gm.groups[prefix]
	gm.mu.RUnlock()

	if !exists {
		return
	}

	group.SetAvailable(pathName, available)

	if gm.onLevelChanged != nil {
		gm.onLevelChanged(prefix, group.GetLevels())
	}
}

// GetGroup returns the quality group for a prefix.
func (gm *GroupManager) GetGroup(prefix string) (*QualityGroup, bool) {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	group, exists := gm.groups[prefix]
	return group, exists
}

// GetGroupForPath returns the quality group that contains the given path.
func (gm *GroupManager) GetGroupForPath(pathName string) (*QualityGroup, bool) {
	prefix, _, ok := ParsePathBitrate(pathName)
	if !ok {
		return nil, false
	}
	return gm.GetGroup(prefix)
}

// GetAllGroups returns all quality groups.
func (gm *GroupManager) GetAllGroups() map[string]*QualityGroup {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	result := make(map[string]*QualityGroup, len(gm.groups))
	for k, v := range gm.groups {
		result[k] = v
	}
	return result
}

// GetActiveGroups returns groups with at least 2 quality levels.
func (gm *GroupManager) GetActiveGroups() map[string]*QualityGroup {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	result := make(map[string]*QualityGroup)
	for k, v := range gm.groups {
		if v.HasMultipleLevels() {
			result[k] = v
		}
	}
	return result
}

// IsABRPath checks if a path name is a virtual ABR path (just the prefix).
func (gm *GroupManager) IsABRPath(pathName string) bool {
	gm.mu.RLock()
	defer gm.mu.RUnlock()

	group, exists := gm.groups[pathName]
	return exists && group.HasMultipleLevels()
}

// GetPrefixForPath returns the ABR prefix for a path, or empty if not an ABR path.
func (gm *GroupManager) GetPrefixForPath(pathName string) string {
	// First check if this is a virtual path (the prefix itself)
	gm.mu.RLock()
	_, exists := gm.groups[pathName]
	gm.mu.RUnlock()

	if exists {
		return pathName
	}

	// Check if this is a quality level path
	prefix, _, ok := ParsePathBitrate(pathName)
	if !ok {
		return ""
	}

	gm.mu.RLock()
	group, exists := gm.groups[prefix]
	gm.mu.RUnlock()

	if exists && group.HasMultipleLevels() {
		return prefix
	}

	return ""
}

