package installbundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type SyncResult struct {
	Copied []string
}

func SyncMissing(source, target string) (SyncResult, error) {
	sourceInfo, err := os.Stat(source)
	if err != nil {
		return SyncResult{}, fmt.Errorf("read config source %s: %w", source, err)
	}
	if !sourceInfo.IsDir() {
		return SyncResult{}, fmt.Errorf("config source is not a directory: %s", source)
	}
	if err := preflightSync(source, target); err != nil {
		return SyncResult{}, err
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return SyncResult{}, fmt.Errorf("create config target %s: %w", target, err)
	}
	result := SyncResult{}
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || relative == "." {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		if _, err := os.Lstat(destination); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if err := copyExclusive(path, destination, info.Mode().Perm()); err != nil {
			return err
		}
		result.Copied = append(result.Copied, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return SyncResult{}, fmt.Errorf("sync configs: %w", err)
	}
	return result, nil
}

func preflightSync(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("config source contains symlink: %s", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("config source contains unsupported file: %s", path)
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := target
		if relative != "." {
			destination = filepath.Join(target, relative)
		}
		targetInfo, err := os.Lstat(destination)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if targetInfo.Mode()&os.ModeSymlink != 0 || targetInfo.IsDir() != info.IsDir() || (!info.IsDir() && !targetInfo.Mode().IsRegular()) {
			return fmt.Errorf("config type conflict at %s", destination)
		}
		return nil
	})
}

func copyExclusive(source, target string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = output.Close()
		if !ok {
			_ = os.Remove(target)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func VerifyChecksum(archivePath, archiveName string, checksums []byte) error {
	expected := ""
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if filepath.Base(name) == archiveName {
			expected = strings.ToLower(fields[0])
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("checksum not found for %s", archiveName)
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", archiveName, expected, actual)
	}
	return nil
}

func ExtractTarGz(archivePath, target string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("open gzip archive: %w", err)
	}
	defer compressed.Close()
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar archive: %w", err)
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path: %s", header.Name)
		}
		destination := filepath.Join(target, name)
		relative, err := filepath.Rel(target, destination)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path: %s", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(destination, 0o700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return err
			}
			output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(output, reader)
			closeErr := output.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("archive contains unsupported entry %s", header.Name)
		}
	}
}
