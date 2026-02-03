package config

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	mountInfoPath  = "/proc/self/mountinfo"
	maxWalkEntries = 4000
	maxWalkDepth   = 3
)

var (
	skipFSTypes = map[string]struct{}{
		"proc":       {},
		"sysfs":      {},
		"devtmpfs":   {},
		"devpts":     {},
		"tmpfs":      {},
		"cgroup":     {},
		"cgroup2":    {},
		"mqueue":     {},
		"pstore":     {},
		"debugfs":    {},
		"tracefs":    {},
		"securityfs": {},
		"fusectl":    {},
		"configfs":   {},
		"autofs":     {},
		"rpc_pipefs": {},
		"squashfs":   {},
		"nsfs":       {},
	}
	skipMountPrefixes = []string{
		"/proc",
		"/sys",
		"/dev",
		"/run",
	}
	errStopWalk = errors.New("stop walk")
)

type mountPoint struct {
	path   string
	fsType string
}

// SuggestDefaultLogPath tries to find a container mount point and returns a
// glob path like /share/logs/*.log or /share/logs/*.gz for setup defaults.
func SuggestDefaultLogPath() string {
	preferred := "/share/logs"
	if info, err := os.Stat(preferred); err == nil && info.IsDir() {
		if ext := detectLogExtension(preferred); ext != "" {
			return filepath.Join(preferred, "*."+ext)
		}
	}

	mountPath := findFirstUsableMount()
	if mountPath == "" {
		return ""
	}
	ext := detectLogExtension(mountPath)
	if ext == "" {
		return ""
	}
	return filepath.Join(mountPath, "*."+ext)
}

func findFirstUsableMount() string {
	mounts := readMountPoints()
	if len(mounts) == 0 {
		return ""
	}
	fallback := ""
	for _, mount := range mounts {
		if mount.path == "/" {
			if fallback == "" && isUsableMount(mount) {
				fallback = mount.path
			}
			continue
		}
		if !isUsableMount(mount) {
			continue
		}
		return mount.path
	}
	return fallback
}

func isUsableMount(mount mountPoint) bool {
	if mount.path == "" {
		return false
	}
	if mount.fsType == "overlay" && mount.path == "/" {
		return false
	}
	if _, blocked := skipFSTypes[mount.fsType]; blocked {
		return false
	}
	for _, prefix := range skipMountPrefixes {
		if mount.path == prefix || strings.HasPrefix(mount.path, prefix+"/") {
			return false
		}
	}
	info, err := os.Stat(mount.path)
	if err != nil || !info.IsDir() {
		return false
	}
	return true
}

func readMountPoints() []mountPoint {
	file, err := os.Open(mountInfoPath)
	if err != nil {
		return nil
	}
	defer file.Close()

	seen := make(map[string]struct{})
	mounts := make([]mountPoint, 0, 8)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, " - ")
		if len(parts) < 2 {
			continue
		}
		left := strings.Fields(parts[0])
		if len(left) < 5 {
			continue
		}
		mountPath := unescapeMountPath(left[4])
		right := strings.Fields(parts[1])
		if len(right) < 1 {
			continue
		}
		fsType := right[0]
		if mountPath == "" {
			continue
		}
		if _, ok := seen[mountPath]; ok {
			continue
		}
		seen[mountPath] = struct{}{}
		mounts = append(mounts, mountPoint{path: mountPath, fsType: fsType})
	}
	return mounts
}

func unescapeMountPath(path string) string {
	replacer := strings.NewReplacer(
		"\\040", " ",
		"\\011", "\t",
		"\\012", "\n",
		"\\134", "\\",
	)
	return replacer.Replace(path)
}

func detectLogExtension(root string) string {
	if hasGlobMatch(root, "*.log") {
		return "log"
	}
	if hasGlobMatch(root, "*.gz") {
		return "gz"
	}

	foundExt := ""
	entryCount := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if walkDepth(root, path) > maxWalkDepth {
				return filepath.SkipDir
			}
			return nil
		}
		entryCount++
		if entryCount > maxWalkEntries {
			return errStopWalk
		}
		name := strings.ToLower(d.Name())
		if strings.HasSuffix(name, ".log") {
			foundExt = "log"
			return errStopWalk
		}
		if foundExt == "" && strings.HasSuffix(name, ".gz") {
			foundExt = "gz"
		}
		return nil
	})
	return foundExt
}

func hasGlobMatch(root, pattern string) bool {
	matches, err := filepath.Glob(filepath.Join(root, pattern))
	if err != nil {
		return false
	}
	for _, match := range matches {
		info, err := os.Stat(match)
		if err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func walkDepth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}
	return strings.Count(rel, string(os.PathSeparator)) + 1
}
