package waypoint

// TODO add description

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type FileType string

// types currently tracked
const (
	FileTypeSymlink  FileType = "symlink"
	FileTypeRegular  FileType = "regular"
	FileTypeDevice   FileType = "device" // modified = major/minor number change
	FileTypeFIFO     FileType = "fifo"   // currently only add/delete tracked
	FileTypeSocket   FileType = "socket" // currently only add/delete tracked
	FileTypeOther    FileType = "other"
	FileTypeModified FileType = "modified" // TODO: use for files whose mode changed, OK?
)

type ChangeType string

const (
	ChangeTypeAdded    ChangeType = "added"
	ChangeTypeDeleted  ChangeType = "deleted"
	ChangeTypeModified ChangeType = "modified"
)

type DiffFileRecord struct {
	Path       string     `json:"path"`
	ChangeType ChangeType `json:"change_type"`
	FileType   FileType   `json:"file_type"`
}

// DiffResult is the JSON printed by `diff`. Other kinds of state go next to
// Filesystem.
type DiffResult struct {
	From       string         `json:"from"`
	To         string         `json:"to"`
	Filesystem FilesystemDiff `json:"filesystem"`
}

type FilesystemDiff struct {
	TargetDir string           `json:"target_dir"`
	Records   []DiffFileRecord `json:"records"`
}

type DiffFS struct {
	overlayDir string

	leftOriginalDir  string
	rightOriginalDir string

	leftLayerIDs  []string
	rightLayerIDs []string

	leftMountPoint  string
	rightMountPoint string
}

type interestingPaths struct {
    prefixes map[string]struct{}
    files    map[string]struct{}
}

func areRegFilesDiff(leftRoot, rightRoot *os.Root, path string) (bool, error) {
	leftFile, err := leftRoot.Open(path)
	if err != nil {
		return false, fmt.Errorf("open left file %s: %w", path, err)
	}
	defer leftFile.Close()

	rightFile, err := rightRoot.Open(path)
	if err != nil {
		return false, fmt.Errorf("open right file %s: %w", path, err)
	}
	defer rightFile.Close()

	// Compare sizes first.
	leftInfo, err := leftFile.Stat()
	if err != nil {
		return false, fmt.Errorf("stat left file %s: %w", path, err)
	}
	rightInfo, err := rightFile.Stat()
	if err != nil {
		return false, fmt.Errorf("stat right file %s: %w", path, err)
	}
	if leftInfo.Size() != rightInfo.Size() {
		return true, nil
	}

	// Compare contents in chunks.
	const chunkSize = 32 * 1024
	leftBuf := make([]byte, chunkSize)
	rightBuf := make([]byte, chunkSize)

	for {
		leftN, leftErr := io.ReadFull(leftFile, leftBuf)
		rightN, rightErr := io.ReadFull(rightFile, rightBuf)

		if !bytes.Equal(leftBuf[:leftN], rightBuf[:rightN]) {
			return true, nil
		}

		if leftErr == nil && rightErr == nil {
			continue
		}
		if (leftErr == io.EOF || leftErr == io.ErrUnexpectedEOF) &&
			(rightErr == io.EOF || rightErr == io.ErrUnexpectedEOF) {
			return false, nil
		}

		if leftErr == io.EOF || leftErr == io.ErrUnexpectedEOF ||
			rightErr == io.EOF || rightErr == io.ErrUnexpectedEOF {
			return true, nil
		}

		if leftErr != nil {
			return false, fmt.Errorf("read left file %s: %w", path, leftErr)
		}
		return false, fmt.Errorf("read right file %s: %w", path, rightErr)
	}
}

func getFileType(root *os.Root, path string) (FileType, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return FileTypeOther, fmt.Errorf("lstat %s: %w", path, err)
	}
	mode := info.Mode()
	if mode.IsRegular() {
		return FileTypeRegular, nil
	}
	if mode&os.ModeSymlink != 0 {
		return FileTypeSymlink, nil
	}
	if mode&os.ModeDevice != 0 {
		return FileTypeDevice, nil
	}
	if mode&os.ModeNamedPipe != 0 {
		return FileTypeFIFO, nil
	}
	if mode&os.ModeSocket != 0 {
		return FileTypeSocket, nil
	}
	return FileTypeOther, nil
}

func processNonDirMod(leftRoot, rightRoot *os.Root, rootViewPath, containerViewPath string, prefixes, interesting *interestingPaths) error {
	if !isInteresting(containerViewPath, interesting) {
		return nil
	}
	leftInfo, err := leftRoot.Lstat(rootViewPath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", rootViewPath, err)
	}
	rightInfo, err := rightRoot.Lstat(rootViewPath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", rootViewPath, err)
	}
	leftFileType, err := getFileType(leftRoot, rootViewPath)
	if err != nil {
		return err
	}
	rightFileType, err := getFileType(rightRoot, rootViewPath)
	if err != nil {
		return err
	}
	// Mode change
	if leftInfo.Mode() != rightInfo.Mode() {
		if leftFileType == rightFileType {
			*detected = append(*detected, DiffFileRecord{
				Path:       containerViewPath,
				ChangeType: ChangeTypeModified,
				FileType:   leftFileType,
			})
		} else {
			*detected = append(*detected, DiffFileRecord{
				Path:       containerViewPath,
				ChangeType: ChangeTypeModified,
				FileType:   FileTypeModified,
			})
		}
		return nil
	}
	// Other changes currently tracked: cmp on regular, symlink target, dev major/minor. TODO support other changes/types
	mod := false
	if leftFileType == FileTypeRegular {
		res, err := areRegFilesDiff(leftRoot, rightRoot, rootViewPath)
		if err != nil {
			return err
		}
		mod = res
	} else if leftFileType == FileTypeSymlink {
		leftTarget, err := leftRoot.Readlink(rootViewPath)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", rootViewPath, err)
		}
		rightTarget, err := rightRoot.Readlink(rootViewPath)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", rootViewPath, err)
		}
		mod = (leftTarget != rightTarget)
	} else if leftFileType == FileTypeDevice {
		leftSys, ok := leftInfo.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("unexpected stat type for %s", rootViewPath)
		}
		rightSys, ok := rightInfo.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("unexpected stat type for %s", rootViewPath)
		}
		leftDev := uint64(leftSys.Rdev)
		rightDev := uint64(rightSys.Rdev)
		mod = (unix.Major(leftDev) != unix.Major(rightDev) ||
			unix.Minor(leftDev) != unix.Minor(rightDev))
	}
	if mod {
		*detected = append(*detected, DiffFileRecord{
			Path:       containerViewPath,
			ChangeType: ChangeTypeModified,
			FileType:   leftFileType,
		})
	}
	return nil
}

func recurseWithChangeType(root *os.Root, rootViewPath, containerViewPath string, detected *[]DiffFileRecord, changeType ChangeType) error {
	info, err := root.Lstat(rootViewPath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", rootViewPath, err)
	}
	if !info.IsDir() {
		fileType, err := getFileType(root, rootViewPath)
		if err != nil {
			return err
		}
		*detected = append(*detected, DiffFileRecord{
			Path:       containerViewPath,
			ChangeType: changeType,
			FileType:   fileType,
		})
		return nil
	}

	dir, err := root.Open(rootViewPath)
	if err != nil {
		return fmt.Errorf("open directory %s: %w", rootViewPath, err)
	}
	dentries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if readErr != nil {
		return fmt.Errorf("read directory %s: %w", rootViewPath, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close directory %s: %w", rootViewPath, closeErr)
	}
	for _, dentry := range dentries {
		rootViewSubpath := filepath.Join(rootViewPath, dentry.Name())
		containerViewSubpath := filepath.Join(containerViewPath, dentry.Name())
		if err := recurseWithChangeType(root, rootViewSubpath, containerViewSubpath, detected, changeType); err != nil {
			return err
		}
	}
	return nil
}

func constructDentryMap(root *os.Root, rootViewDir string) (map[string]os.DirEntry, error) {
	dir, err := root.Open(rootViewDir)
	if err != nil {
		return nil, fmt.Errorf("open directory %s: %w", rootViewDir, err)
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read directory %s: %w", rootViewDir, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close directory %s: %w", rootViewDir, closeErr)
	}
	nameMap := make(map[string]os.DirEntry, len(entries))
	for _, entry := range entries {
		nameMap[entry.Name()] = entry
	}
	return nameMap, nil
}

func walkFS(leftRoot, rightRoot *os.Root, rootViewDir, containerViewDir string, interesting *interestingPaths, detected *[]DiffFileRecord) error {
	// note: won't follow symlink dirs
	leftNameMap, err := constructDentryMap(leftRoot, rootViewDir)
	if err != nil {
		return err
	}
	rightNameMap, err := constructDentryMap(rightRoot, rootViewDir)
	if err != nil {
		return err
	}
	for name, leftDentry := range leftNameMap {
		containerViewSubpath := filepath.Join(containerViewDir, name)
		rootViewSubpath := filepath.Join(rootViewDir, name)
		rightDentry, exists := rightNameMap[name]
		if !exists {
			// Left-only entry -> deleted
			if err := recurseWithChangeType(leftRoot, rootViewSubpath, containerViewSubpath, detected, ChangeTypeDeleted); err != nil {
				return err
			}
			continue
		}
		// both directories, so recurse
		if leftDentry.IsDir() && rightDentry.IsDir() {
			if err := walkFS(leftRoot, rightRoot, rootViewSubpath, containerViewSubpath, interesting, detected); err != nil {
				return err
			}
			continue
		}
		// Just one side is directory -> left is deleted, right is added
		if (leftDentry.IsDir() && !rightDentry.IsDir()) || (!leftDentry.IsDir() && rightDentry.IsDir()) {
			if err := recurseWithChangeType(leftRoot, rootViewSubpath, containerViewSubpath, detected, ChangeTypeDeleted); err != nil {
				return err
			}
			if err := recurseWithChangeType(rightRoot, rootViewSubpath, containerViewSubpath, detected, ChangeTypeAdded); err != nil {
				return err
			}
			continue
		}
		// Neither directories -> check for modification
		if err := processNonDirMod(leftRoot, rightRoot, rootViewSubpath, containerViewSubpath, interesting, detected); err != nil {
			return err
		}
	}
	// Right-only entries -> added
	for name := range rightNameMap {
		if _, exists := leftNameMap[name]; exists {
			continue
		}
		containerViewSubpath := filepath.Join(containerViewDir, name)
		rootViewSubpath := filepath.Join(rootViewDir, name)
		if err := recurseWithChangeType(rightRoot, rootViewSubpath, containerViewSubpath, detected, ChangeTypeAdded); err != nil {
			return err
		}
		continue
	}
	return nil
}

func isInteresting(path string, interesting *interestingPaths) bool {
	if _, exists := interesting.files[path]; exists {
		return true
	}
	for current := path; ; current = filepath.Dir(current) {
		if _, exists := interesting.prefixes[current]; exists {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return false
}

func collectInterestingDir(rootDirPath, containerDirPath string, interesting *interestingPaths) error {
	dentries, err := os.ReadDir(rootDirPath)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", rootDirPath, err)
	}
	for _, dentry := range dentries {
		rootDentryPath := filepath.Join(rootDirPath, dentry.Name())
		containerDentryPath := filepath.Join(containerDirPath, dentry.Name())
		if !dentry.IsDir() {
			whiteout, err := isWhiteout(rootDentryPath)
			if err != nil {
				return err
			}
			if whiteout {
				interesting.prefixes[containerDentryPath] = struct{}{}
			}
			interesting.files[containerDentryPath] = struct{}{}
		} else {
			opaque, err := isOpaque(rootDentryPath)
			if err != nil {
				return err
			}
			if opaque {
				interesting.prefixes[containerDentryPath] = struct{}{}
				return nil
			}
			if err := collectInterestingDir(rootDentryPath, containerDentryPath, interesting); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) getInteresting(d *DiffFS, interesting *interestingPaths) error {
	// NOTE: assumes originalDir is the same for both checkpoints
	// should first check if d.leftOriginalDir != d.rightOriginalDir, if so should need to collect interesting paths from original dir too (everything is interesting)
	// maybe have path where if interesting is nil, check everything? so if original dirs differ, just check everything
	leftLayers := d.leftLayerIDs
	rightLayers := d.rightLayerIDs
	lastCommonIdx := -1
	for i := 0; i < len(leftLayers) && i < len(rightLayers); i++ {
		if leftLayers[i] != rightLayers[i] {
			break
		}
		lastCommonIdx = i
	}
	unsharedCkpts := leftLayers[lastCommonIdx+1:]
	unsharedCkpts = append(unsharedCkpts, rightLayers[lastCommonIdx+1:]...)

	for _, ckptID := range unsharedCkpts {
		upperDirPath := m.checkpointUpperDir(ckptID)
		if err := collectInterestingDir(upperDirPath, "/", interesting); err != nil {
			return err
		}
	}
	return nil
}

func existsTargetDir(root *os.Root, path string) (bool, error) {
	info, err := root.Lstat(path) // requires nonsymlink dir
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("lstat %s: %w", path, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory", path)
	}
	return true, nil
}

func newDiffFS(m *Manager, overlayDir, leftCkptID, rightCkptID string) (*DiffFS, error) {
	leftMetadata, err := m.loadMetadata(leftCkptID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", leftCkptID, err)
	}
	rightMetadata, err := m.loadMetadata(rightCkptID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", rightCkptID, err)
	}
	if leftMetadata.Status != CheckpointStatusReady {
		return nil, fmt.Errorf("checkpoint %s is not ready (status: %s)", leftCkptID, leftMetadata.Status)
	}
	if rightMetadata.Status != CheckpointStatusReady {
		return nil, fmt.Errorf("checkpoint %s is not ready (status: %s)", rightCkptID, rightMetadata.Status)
	}
	leftLowerIDs := append([]string(nil), leftMetadata.LayerIDs...)
	rightLowerIDs := append([]string(nil), rightMetadata.LayerIDs...)

	leftOriginalDir := leftMetadata.OriginalDir
	if leftOriginalDir == "" {
		leftOriginalDir = m.originalDir
	}
	rightOriginalDir := rightMetadata.OriginalDir
	if rightOriginalDir == "" {
		rightOriginalDir = m.originalDir
	}
	return &DiffFS{
		overlayDir:       overlayDir,
		leftLayerIDs:     leftLowerIDs,
		rightLayerIDs:    rightLowerIDs,
		leftOriginalDir:  leftOriginalDir,
		rightOriginalDir: rightOriginalDir,
		leftMountPoint:   filepath.Join(overlayDir, "left_mount"),
		rightMountPoint:  filepath.Join(overlayDir, "right_mount"),
	}, nil
}

func normalizeTargetDir(targetDir string) (string, error) {
	targetDir = strings.TrimLeft(targetDir, "/")
	clean := filepath.Clean(targetDir)

	// Treat / as relative to the container root.
	if clean == "." {
		return "/", nil
	}
	if clean == ".." ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("target directory must not escape the filesystem root")
	}
	return "/" + clean, nil
}

func computeTargetHash(targetDir string) (string, error) {
	normalized, err := normalizeTargetDir(targetDir)
	if err != nil {
		return "", err
	}
	if normalized == "" {
		return "root", nil
	}
	hash := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(hash[:8]), nil
}

func computeTargetTempDir(ckptsDiffDir string, targetDir string) (string, error) {
	targetCacheKey, err := computeTargetHash(targetDir)
	if err != nil {
		return "", err
	}
	temp_id := "temp-" + targetCacheKey
	targetTempDir := filepath.Join(ckptsDiffDir, temp_id)
	return targetTempDir, nil
}

func computeResultPath(ckptsDiffDir string, targetDir string) (string, error) {
	targetCacheKey, err := computeTargetHash(targetDir)
	if err != nil {
		return "", err
	}
	res_id := "result-" + targetCacheKey
	targetCacheResult := filepath.Join(ckptsDiffDir, res_id+".json")
	return targetCacheResult, nil
}

func computeCkptsDiffID(leftCkptID, rightCkptID string) string {
	sum := sha256.Sum256([]byte(leftCkptID + "/" + rightCkptID)) // ensures unique pair because ckptIDs can't include /
	hash := hex.EncodeToString(sum[:8])
	return fmt.Sprintf("%.5s-%.5s-%s", leftCkptID, rightCkptID, hash) // add a bit of ckpt names for readability
}

func (m *Manager) setupDiffFS(ckptIDFirst, ckptIDSecond, normalizedTargetDir string) (*DiffFS, []byte, error) {
	ckptsDiffID := computeCkptsDiffID(ckptIDFirst, ckptIDSecond)
	ckptsDiffDir := m.diffDir(ckptsDiffID)
	targetCacheResult, err := computeResultPath(ckptsDiffDir, normalizedTargetDir)
	if err != nil {
		return nil, nil, err
	}
	targetTempDir, err := computeTargetTempDir(ckptsDiffDir, normalizedTargetDir)
	if err != nil {
		return nil, nil, err
	}

	var cachedData []byte
	var d *DiffFS
	if err := m.withSessionLock(func() error {
		_, errCkpt := os.Stat(ckptsDiffDir)
		_, errTemp := os.Stat(targetTempDir)
		_, errRes := os.Stat(targetCacheResult)
		if errRes == nil {
			// case 1: already have cached result
			// TODO further validation of cache before try to read?
			var err error
			cachedData, err = os.ReadFile(targetCacheResult)
			if err != nil {
				return fmt.Errorf("read cached diff result: %w", err)
			}
			return nil
		} else if !os.IsNotExist(errRes) {
			return errRes
		}
		if errTemp == nil {
			// case 2: have temp dir for this target but no result yet -> computation already in progress, fail
			return fmt.Errorf("diff workspace %s on target %s already exists but results json not found", targetTempDir, normalizedTargetDir)
		} else if !os.IsNotExist(errTemp) {
			return errTemp
		}
		if errCkpt != nil {
			if os.IsNotExist(errCkpt) {
				// case 3: don't have any diffs on these ckpts yet
				if err := os.MkdirAll(ckptsDiffDir, 0o755); err != nil {
					return fmt.Errorf("mkdir %s failed: %w", ckptsDiffDir, err)
				}
			} else {
				return errCkpt
			}
		}
		newD, err := newDiffFS(m, targetTempDir, ckptIDFirst, ckptIDSecond)
		if err != nil {
			return err
		}
		d = newD
		// case 3,4: have ckpt diff dir, but not yet on this target
		if err := os.Mkdir(targetTempDir, 0o700); err != nil {
			return fmt.Errorf("mkdir %s failed: %w", targetTempDir, err)
		}
		for _, dir := range []string{d.leftMountPoint, d.rightMountPoint} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				_ = os.RemoveAll(targetTempDir)
				return fmt.Errorf("mkdir %s failed: %w", dir, err)
			}
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if cachedData != nil {
		return nil, cachedData, nil
	}

	// Mount
	// layerIDs is ordered oldest to newest, overlayLowerDirs inverts this order to give priority to newest
	leftOverlayLowers := overlayLowerDirs(m.checkpointsDir(), d.leftLayerIDs, d.leftOriginalDir)
	rightOverlayLowers := overlayLowerDirs(m.checkpointsDir(), d.rightLayerIDs, d.rightOriginalDir)

	optionsLeft := fmt.Sprintf("lowerdir=%s", strings.Join(leftOverlayLowers, ":"))
	if err := unix.Mount("overlay", d.leftMountPoint, "overlay", unix.MS_RDONLY, optionsLeft); err != nil {
		_ = os.RemoveAll(d.overlayDir)
		return nil, nil, fmt.Errorf("mount overlay at %s failed: %w", d.leftMountPoint, err)
	}

	optionsRight := fmt.Sprintf("lowerdir=%s", strings.Join(rightOverlayLowers, ":"))
	if err := unix.Mount("overlay", d.rightMountPoint, "overlay", unix.MS_RDONLY, optionsRight); err != nil {
		_ = unix.Unmount(d.leftMountPoint, unix.MNT_DETACH)
		_ = os.RemoveAll(d.overlayDir)
		return nil, nil, fmt.Errorf("mount overlay at %s failed: %w", d.rightMountPoint, err)
	}

	return d, nil, nil
}

// Entry point for diff
func (m *Manager) PrintDiff(ckptIDFirst, ckptIDSecond, targetDir string) error {
	normalizedTargetDir, err := normalizeTargetDir(targetDir)
	if err != nil {
		return err
	}
	if err := validateCheckpointID(ckptIDFirst); err != nil {
		return fmt.Errorf("invalid checkpoint ID %s: %w", ckptIDFirst, err)
	}
	if err := validateCheckpointID(ckptIDSecond); err != nil {
		return fmt.Errorf("invalid checkpoint ID %s: %w", ckptIDSecond, err)
	}
	if ckptIDFirst == ckptIDSecond {
		// TODO: error or output empty diff?
		return fmt.Errorf("cannot diff the same checkpoint %s", ckptIDFirst)
	}

	d, cachedData, err := m.setupDiffFS(ckptIDFirst, ckptIDSecond, normalizedTargetDir)
	if err != nil {
		return err
	}
	if cachedData != nil {
		if _, err := os.Stdout.Write(append(cachedData, '\n')); err != nil {
			return fmt.Errorf("write cached diff result to stdout: %w", err)
		}
		return nil
	}

	defer func() {
		_ = unix.Unmount(d.rightMountPoint, unix.MNT_DETACH)
		_ = unix.Unmount(d.leftMountPoint, unix.MNT_DETACH)
		_ = os.RemoveAll(d.overlayDir)
	}()

	// TODO: openroot will report error on absolute symlinks (doesn't resolve as relative to container root)
	leftRoot, err := os.OpenRoot(d.leftMountPoint)
	if err != nil {
		return err
	}
	defer leftRoot.Close()
	rightRoot, err := os.OpenRoot(d.rightMountPoint)
	if err != nil {
		return err
	}
	defer rightRoot.Close()
	targetForRoot, err := cleanRootPath(normalizedTargetDir)
	if err != nil {
		return err
	}

	leftExists, err := existsTargetDir(leftRoot, targetForRoot)
	if err != nil {
		return err
	}
	rightExists, err := existsTargetDir(rightRoot, targetForRoot)
	if err != nil {
		return err
	}

	interesting := &interestingPaths{
		prefixes: make(map[string]struct{}),
		files:    make(map[string]struct{}),
	}
	if err := m.getInteresting(d, interesting); err != nil {
		return err
	}

	detected := make([]DiffFileRecord, 0)
	switch {
	case !leftExists && !rightExists:
	case leftExists && !rightExists:
		if err := recurseWithChangeType(leftRoot, targetForRoot, normalizedTargetDir, &detected, ChangeTypeDeleted); err != nil {
			return err
		}
	case !leftExists && rightExists:
		if err := recurseWithChangeType(rightRoot, targetForRoot, normalizedTargetDir, &detected, ChangeTypeAdded); err != nil {
			return err
		}
	default:
		if err := walkFS(leftRoot, rightRoot, targetForRoot, normalizedTargetDir, interesting, &detected); err != nil {
			return err
		}
	}

	sort.Slice(detected, func(i, j int) bool {
		return detected[i].Path < detected[j].Path
	})

	ckptsDiffID := computeCkptsDiffID(ckptIDFirst, ckptIDSecond)
	ckptsDiffDir := m.diffDir(ckptsDiffID)
	resultPath, err := computeResultPath(ckptsDiffDir, normalizedTargetDir)
	if err != nil {
		return err
	}

	result := DiffResult{
		From: ckptIDFirst,
		To:   ckptIDSecond,
		Filesystem: FilesystemDiff{
			TargetDir: normalizedTargetDir,
			Records:   detected,
		},
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal diff result: %w", err)
	}
	if err := m.withSessionLock(func() error {
		return atomicWriteFile(resultPath, data, 0o644)
	}); err != nil {
		return err
	}
	if _, err := os.Stdout.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write diff result to stdout: %w", err)
	}

	return nil
}
