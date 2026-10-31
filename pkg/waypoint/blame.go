package waypoint

// TODO add description

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sort"
	"syscall"

	"golang.org/x/sys/unix"
)

type BlamedCkptRecord struct {
	CheckpointId    string           `json:"checkpoint_id"`
	// TODO: add field containing the actual changes from previous?
}

func hideVisibleChildren(relPath string, visible, updated map[string]bool) error {
	for interestingRelPath, isVisible := range visible {
		if !isVisible || updated[interestingRelPath] {
			continue
		}
		childRelPath, err := filepath.Rel(relPath, interestingRelPath)
		if err != nil {
			return err
		}
		if childRelPath == "." || childRelPath == ".." ||
			strings.HasPrefix(childRelPath, ".."+string(filepath.Separator)) {
			continue
		}
		visible[interestingRelPath] = false
		updated[interestingRelPath] = true
	}
	return nil
}

func blameDir(fsDir, relDir string, updated, visible map[string]bool) error {
	dentries, err := os.ReadDir(fsDir)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", fsDir, err)
	}
	for _, dentry := range dentries {
		fsPath := filepath.Join(fsDir, dentry.Name())
		relPath := filepath.Join(relDir, dentry.Name())
		if !dentry.IsDir() {
			whiteout, err := isWhiteout(fsPath)
			if err != nil {
				return err
			}
			if whiteout {
				if _, exists := visible[relPath]; exists {
					visible[relPath] = !whiteout
					updated[relPath] = true
				} else if err := hideVisibleChildren(relPath, visible, updated); err != nil {
					return err
				}	
			} else if _, exists := visible[relPath]; exists {
				visible[relPath] = true
				updated[relPath] = true
			} 
		} else {
			if err := blameDir(fsPath, relPath, updated, visible); err != nil {
				return err
			}
			opaque, err := isOpaque(fsPath)
			if err != nil {
				return err
			}
			if opaque {
				if err := hideVisibleChildren(relPath, visible, updated); err != nil {
					return err
				}
			}

		}
	}
	return nil
}

func (m *Manager) advanceBlame(ckptIDs []string, visible map[string]bool, blame map[string][]string) error {
	for _, ckptID := range ckptIDs {
		upperDirPath := m.checkpointUpperDir(ckptID)
		updated := make(map[string]bool)
		if err := blameDir(upperDirPath, "", updated, visible); err != nil {
			return err
		}
		if blame != nil {
			for relPath := range updated {
				blame[relPath] = append(blame[relPath], ckptID)
			}
		}
	}
	return nil
}

func advanceOG(ogDir string, visible map[string]bool) error {
	updated := make(map[string]bool)
	if err := blameDir(ogDir, "", updated, visible); err != nil {
		return err
	}
	return nil
}

func (m *Manager) blame(d *Diff, visible map[string]bool) (map[string][]string, map[string][]string, error) {
	// determine if parent-child or leaf-leaf relationship
	metadata, err := m.loadMetadata(ckptID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", ckptID, err)
	}
	ckptLayers := append([]string(nil), leftMetadata.LayerIDs...)
	originalDir := metadata.OriginalDir
	if err := advanceOG(originalDir, visible, path); err != nil {
		return nil, nil, err
	}
	if err := m.advanceBlame(ckptLayers, visible, path, blamed); err != nil {
		return nil, nil, err
	}
	return leftBlame, rightBlame, nil
}

// very similar to diff.go normalizeTargetDir, refactor?
func normalizePath(targetDir string) (string, error) {
    targetDir = strings.TrimLeft(targetDir, "/")
	clean := filepath.Clean(targetDir)

	if clean == "." {
        return "", fmt.Errorf("path is for root directory, blame does not support directory targets")
    }
    if clean == ".." ||
        strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
        return "", fmt.Errorf("path must not escape the filesystem root")
    }
    return "/" + clean, nil
}

// Entry point for blame
func (m *Manager) PrintBlame(ckptID, path string) error {
	// need to get normalized path
	// walk through upper layers looking for it
	normalizedPath, err := normalizePath(targetDir)
	if err != nil {
		return err
	}

	blamed := make([]BlamedCkptRecord, 0)

	metadata, err := m.loadMetadata(ckptID)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata for checkpoint %s: %w", ckptID, err)
	}
	ckptLayers := append([]string(nil), leftMetadata.LayerIDs...)
	originalDir := metadata.OriginalDir
	if err := advanceOG(originalDir, visible, path); err != nil {
		return nil, nil, err
	}
	if err := m.advanceBlame(ckptLayers, visible, path, blamed); err != nil {
		return nil, nil, err
	}
	return leftBlame, rightBlame, nil

	switch {
	case !leftExists && !rightExists:
	case leftExists && !rightExists:
		if err := recurseWithChangeType(leftTargetDir, normalizedTargetDir, &detected, ChangeTypeDeleted); err != nil {
			return err
		}
	case !leftExists && rightExists:
		if err := recurseWithChangeType(rightTargetDir, normalizedTargetDir, &detected, ChangeTypeAdded); err != nil {
			return err
		}
	default:
		if err := walkFS(leftTargetDir, rightTargetDir, normalizedTargetDir, &detected); err != nil {
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
		TargetDir: normalizedTargetDir,
		Records: detected,
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
