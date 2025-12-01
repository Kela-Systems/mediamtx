// Package abr implements adaptive bitrate streaming for WebRTC.
package abr

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// QualityLevel represents a single quality/bitrate level of a stream.
type QualityLevel struct {
	// Path is the full path name (e.g., "mystream_2000")
	Path string
	// Bitrate is the target bitrate in bits per second
	Bitrate int
	// Available indicates if this quality level is currently available
	Available bool
}

// QualityLevels is a sorted slice of quality levels (highest bitrate first).
type QualityLevels []QualityLevel

func (ql QualityLevels) Len() int           { return len(ql) }
func (ql QualityLevels) Less(i, j int) bool { return ql[i].Bitrate > ql[j].Bitrate } // Descending
func (ql QualityLevels) Swap(i, j int)      { ql[i], ql[j] = ql[j], ql[i] }

// QualityGroup represents a group of paths that are different quality levels
// of the same content (sharing a common prefix).
type QualityGroup struct {
	// Prefix is the common path prefix (e.g., "mystream")
	Prefix string
	// Levels contains all quality levels, sorted by bitrate (highest first)
	Levels QualityLevels

	mu sync.RWMutex
}

// pathBitratePattern matches paths ending with _<number> or /<number>
// where the number represents bitrate in kbps or bps
var pathBitratePattern = regexp.MustCompile(`^(.+)[_/](\d+)$`)

// ParsePathBitrate extracts prefix and bitrate from a path name.
// Returns prefix, bitrate (in bps), and whether the path matches the pattern.
// Examples:
//   - "mystream_2000" -> "mystream", 2000000, true (assumes kbps)
//   - "live/stream_500" -> "live/stream", 500000, true
//   - "mystream" -> "", 0, false
func ParsePathBitrate(pathName string) (prefix string, bitrate int, ok bool) {
	matches := pathBitratePattern.FindStringSubmatch(pathName)
	if matches == nil {
		return "", 0, false
	}

	prefix = matches[1]
	bitrateNum, err := strconv.Atoi(matches[2])
	if err != nil {
		return "", 0, false
	}

	// Heuristic: if number is less than 100000, assume it's kbps, otherwise bps
	if bitrateNum < 100000 {
		bitrate = bitrateNum * 1000 // Convert kbps to bps
	} else {
		bitrate = bitrateNum
	}

	return prefix, bitrate, true
}

// BuildPathName creates a path name from prefix and bitrate.
func BuildPathName(prefix string, bitrateKbps int) string {
	return prefix + "_" + strconv.Itoa(bitrateKbps)
}

// NewQualityGroup creates a new QualityGroup for the given prefix.
func NewQualityGroup(prefix string) *QualityGroup {
	return &QualityGroup{
		Prefix: prefix,
		Levels: make(QualityLevels, 0),
	}
}

// AddLevel adds or updates a quality level.
func (qg *QualityGroup) AddLevel(pathName string, bitrate int) {
	qg.mu.Lock()
	defer qg.mu.Unlock()

	// Check if level already exists
	for i, level := range qg.Levels {
		if level.Path == pathName {
			qg.Levels[i].Bitrate = bitrate
			qg.Levels[i].Available = true
			sort.Sort(qg.Levels)
			return
		}
	}

	// Add new level
	qg.Levels = append(qg.Levels, QualityLevel{
		Path:      pathName,
		Bitrate:   bitrate,
		Available: true,
	})
	sort.Sort(qg.Levels)
}

// RemoveLevel removes a quality level.
func (qg *QualityGroup) RemoveLevel(pathName string) {
	qg.mu.Lock()
	defer qg.mu.Unlock()

	for i, level := range qg.Levels {
		if level.Path == pathName {
			qg.Levels = append(qg.Levels[:i], qg.Levels[i+1:]...)
			return
		}
	}
}

// SetAvailable sets the availability of a quality level.
func (qg *QualityGroup) SetAvailable(pathName string, available bool) {
	qg.mu.Lock()
	defer qg.mu.Unlock()

	for i, level := range qg.Levels {
		if level.Path == pathName {
			qg.Levels[i].Available = available
			return
		}
	}
}

// GetLevels returns a copy of all quality levels.
func (qg *QualityGroup) GetLevels() QualityLevels {
	qg.mu.RLock()
	defer qg.mu.RUnlock()

	result := make(QualityLevels, len(qg.Levels))
	copy(result, qg.Levels)
	return result
}

// GetAvailableLevels returns only available quality levels.
func (qg *QualityGroup) GetAvailableLevels() QualityLevels {
	qg.mu.RLock()
	defer qg.mu.RUnlock()

	result := make(QualityLevels, 0, len(qg.Levels))
	for _, level := range qg.Levels {
		if level.Available {
			result = append(result, level)
		}
	}
	return result
}

// SelectLevel selects the best quality level for the given bandwidth.
// Returns the selected level and whether a level was found.
// The selection uses a buffer margin to avoid frequent switching.
func (qg *QualityGroup) SelectLevel(bandwidthBps int, currentPath string) (QualityLevel, bool) {
	qg.mu.RLock()
	defer qg.mu.RUnlock()

	available := make(QualityLevels, 0, len(qg.Levels))
	for _, level := range qg.Levels {
		if level.Available {
			available = append(available, level)
		}
	}

	if len(available) == 0 {
		return QualityLevel{}, false
	}

	// Find current level index
	currentIdx := -1
	for i, level := range available {
		if level.Path == currentPath {
			currentIdx = i
			break
		}
	}

	// Selection strategy with hysteresis:
	// - To switch UP: bandwidth must be > 1.2x target bitrate
	// - To switch DOWN: bandwidth must be < 0.8x target bitrate
	// This prevents oscillation near threshold boundaries

	const (
		upThreshold   = 1.2
		downThreshold = 0.8
	)

	// Find the highest quality level that fits within bandwidth
	selectedIdx := len(available) - 1 // Start with lowest quality

	for i, level := range available {
		// Use different thresholds based on whether we're switching up or down
		if currentIdx >= 0 && i < currentIdx {
			// Considering switching UP - use higher threshold
			if float64(bandwidthBps) >= float64(level.Bitrate)*upThreshold {
				selectedIdx = i
				break
			}
		} else if currentIdx >= 0 && i > currentIdx {
			// Considering switching DOWN - use lower threshold
			if float64(bandwidthBps) >= float64(level.Bitrate)*downThreshold {
				selectedIdx = i
				break
			}
		} else {
			// No current level or same level - use standard threshold
			if float64(bandwidthBps) >= float64(level.Bitrate) {
				selectedIdx = i
				break
			}
		}
	}

	return available[selectedIdx], true
}

// IsEmpty returns true if there are no quality levels.
func (qg *QualityGroup) IsEmpty() bool {
	qg.mu.RLock()
	defer qg.mu.RUnlock()
	return len(qg.Levels) == 0
}

// HasMultipleLevels returns true if there are at least 2 quality levels.
func (qg *QualityGroup) HasMultipleLevels() bool {
	qg.mu.RLock()
	defer qg.mu.RUnlock()
	return len(qg.Levels) >= 2
}

// HighestLevel returns the highest quality level available.
func (qg *QualityGroup) HighestLevel() (QualityLevel, bool) {
	qg.mu.RLock()
	defer qg.mu.RUnlock()

	for _, level := range qg.Levels {
		if level.Available {
			return level, true
		}
	}
	return QualityLevel{}, false
}

// LowestLevel returns the lowest quality level available.
func (qg *QualityGroup) LowestLevel() (QualityLevel, bool) {
	qg.mu.RLock()
	defer qg.mu.RUnlock()

	for i := len(qg.Levels) - 1; i >= 0; i-- {
		if qg.Levels[i].Available {
			return qg.Levels[i], true
		}
	}
	return QualityLevel{}, false
}

// ContainsPath checks if the group contains a specific path.
func (qg *QualityGroup) ContainsPath(pathName string) bool {
	qg.mu.RLock()
	defer qg.mu.RUnlock()

	for _, level := range qg.Levels {
		if level.Path == pathName {
			return true
		}
	}
	return false
}

// String returns a string representation of the quality group.
func (qg *QualityGroup) String() string {
	qg.mu.RLock()
	defer qg.mu.RUnlock()

	var parts []string
	for _, level := range qg.Levels {
		status := "available"
		if !level.Available {
			status = "unavailable"
		}
		parts = append(parts, level.Path+"@"+strconv.Itoa(level.Bitrate/1000)+"kbps("+status+")")
	}
	return qg.Prefix + ": [" + strings.Join(parts, ", ") + "]"
}

