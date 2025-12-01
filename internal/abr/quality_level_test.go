package abr

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParsePathBitrate(t *testing.T) {
	tests := []struct {
		name           string
		pathName       string
		expectPrefix   string
		expectBitrate  int
		expectOK       bool
	}{
		{
			name:          "simple path with kbps",
			pathName:      "mystream_2000",
			expectPrefix:  "mystream",
			expectBitrate: 2000000, // 2000 kbps = 2000000 bps
			expectOK:      true,
		},
		{
			name:          "nested path with kbps",
			pathName:      "live/video_1000",
			expectPrefix:  "live/video",
			expectBitrate: 1000000,
			expectOK:      true,
		},
		{
			name:          "path with bps (large number)",
			pathName:      "stream_2000000",
			expectPrefix:  "stream",
			expectBitrate: 2000000,
			expectOK:      true,
		},
		{
			name:          "path with underscore and number",
			pathName:      "my_stream_500",
			expectPrefix:  "my_stream",
			expectBitrate: 500000,
			expectOK:      true,
		},
		{
			name:       "path without number",
			pathName:   "mystream",
			expectOK:   false,
		},
		{
			name:       "path with text suffix",
			pathName:   "mystream_hd",
			expectOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prefix, bitrate, ok := ParsePathBitrate(tt.pathName)
			require.Equal(t, tt.expectOK, ok)
			if ok {
				require.Equal(t, tt.expectPrefix, prefix)
				require.Equal(t, tt.expectBitrate, bitrate)
			}
		})
	}
}

func TestQualityGroupSelectLevel(t *testing.T) {
	group := NewQualityGroup("test")
	group.AddLevel("test_2000", 2000000)
	group.AddLevel("test_1000", 1000000)
	group.AddLevel("test_500", 500000)

	tests := []struct {
		name          string
		bandwidth     int
		currentPath   string
		expectPath    string
		expectFound   bool
	}{
		{
			name:        "high bandwidth selects highest quality",
			bandwidth:   5000000,
			currentPath: "",
			expectPath:  "test_2000",
			expectFound: true,
		},
		{
			name:        "medium bandwidth selects medium quality",
			bandwidth:   1200000,
			currentPath: "",
			expectPath:  "test_1000",
			expectFound: true,
		},
		{
			name:        "low bandwidth selects lowest quality",
			bandwidth:   400000,
			currentPath: "",
			expectPath:  "test_500",
			expectFound: true,
		},
		{
			name:        "hysteresis prevents rapid upward switch",
			bandwidth:   2100000, // Just above 2000kbps but below 1.2x threshold
			currentPath: "test_1000",
			expectPath:  "test_1000", // Should stay at current level
			expectFound: true,
		},
		{
			name:        "switches up when bandwidth is well above threshold",
			bandwidth:   2500000, // 1.25x of 2000kbps
			currentPath: "test_1000",
			expectPath:  "test_2000",
			expectFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level, found := group.SelectLevel(tt.bandwidth, tt.currentPath)
			require.Equal(t, tt.expectFound, found)
			if found {
				require.Equal(t, tt.expectPath, level.Path)
			}
		})
	}
}

func TestQualityGroupAddRemove(t *testing.T) {
	group := NewQualityGroup("test")

	// Initially empty
	require.True(t, group.IsEmpty())
	require.False(t, group.HasMultipleLevels())

	// Add first level
	group.AddLevel("test_2000", 2000000)
	require.False(t, group.IsEmpty())
	require.False(t, group.HasMultipleLevels())

	// Add second level
	group.AddLevel("test_1000", 1000000)
	require.True(t, group.HasMultipleLevels())

	// Check levels are sorted by bitrate (highest first)
	levels := group.GetLevels()
	require.Len(t, levels, 2)
	require.Equal(t, "test_2000", levels[0].Path)
	require.Equal(t, "test_1000", levels[1].Path)

	// Remove one level
	group.RemoveLevel("test_2000")
	require.False(t, group.HasMultipleLevels())

	// Remove last level
	group.RemoveLevel("test_1000")
	require.True(t, group.IsEmpty())
}

func TestQualityGroupAvailability(t *testing.T) {
	group := NewQualityGroup("test")
	group.AddLevel("test_2000", 2000000)
	group.AddLevel("test_1000", 1000000)
	group.AddLevel("test_500", 500000)

	// All levels available initially
	available := group.GetAvailableLevels()
	require.Len(t, available, 3)

	// Mark one as unavailable
	group.SetAvailable("test_2000", false)
	available = group.GetAvailableLevels()
	require.Len(t, available, 2)

	// Selecting with high bandwidth should now pick medium
	level, found := group.SelectLevel(5000000, "")
	require.True(t, found)
	require.Equal(t, "test_1000", level.Path)

	// Mark it available again
	group.SetAvailable("test_2000", true)
	available = group.GetAvailableLevels()
	require.Len(t, available, 3)
}

func TestBuildPathName(t *testing.T) {
	path := BuildPathName("mystream", 2000)
	require.Equal(t, "mystream_2000", path)

	path = BuildPathName("live/video", 500)
	require.Equal(t, "live/video_500", path)
}

