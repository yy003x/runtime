package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yy003x/runtime/internal/domain/identity"
)

const (
	sessionTrashMoveVersion = 1
	maxTrashMoveBytes       = 16 << 10
)

type sessionTrashMoveJournal struct {
	Version        int    `json:"version"`
	SessionID      string `json:"session_id"`
	TargetRelative string `json:"target_relative"`
	RootDevice     uint64 `json:"root_device"`
	RootInode      uint64 `json:"root_inode"`
}

func (store *Store) durableMoveSession(
	sessionID string,
	target string,
) (string, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return "", err
	}
	if err := store.ensure(); err != nil {
		return "", err
	}
	if pending, err := store.readTrashMoveJournal(sessionID); err == nil {
		if err := store.finishTrashMove(pending); err != nil {
			return "", fmt.Errorf(
				"Session trash move is pending durable recovery: %w", err,
			)
		}
		return filepath.Join(store.sessionsDir, pending.TargetRelative), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	target = filepath.Clean(target)
	targetRelative, err := filepath.Rel(store.sessionsDir, target)
	if err != nil {
		return "", err
	}
	journal := sessionTrashMoveJournal{
		Version:        sessionTrashMoveVersion,
		SessionID:      sessionID,
		TargetRelative: targetRelative,
	}
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return "", err
	}
	rootIdentity, err := root.identity()
	if err != nil {
		root.close()
		return "", err
	}
	if err := root.close(); err != nil {
		return "", err
	}
	journal.RootDevice = rootIdentity.dev
	journal.RootInode = rootIdentity.ino
	if err := validateTrashMoveJournal(journal); err != nil {
		return "", err
	}
	if err := store.persistTrashMoveJournal(journal); err != nil {
		return "", err
	}
	if err := store.finishTrashMove(journal); err != nil {
		return "", fmt.Errorf(
			"Session trash move is pending durable recovery: %w", err,
		)
	}
	return target, nil
}

func (store *Store) recoverTrashMoves() (bool, error) {
	directory, err := store.openPinnedDirectory(store.moveDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	entries, err := directory.entries()
	closeErr := directory.close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	recovered := false
	for _, entry := range entries {
		if entry.isRegular() && isOwnedAtomicTempName(entry.name) {
			continue
		}
		if !entry.isRegular() ||
			entry.nlink != 1 ||
			filepath.Ext(entry.name) != ".json" {
			return false, fmt.Errorf(
				"Session trash move journal contains unsupported entry %q",
				entry.name,
			)
		}
		sessionID := strings.TrimSuffix(entry.name, ".json")
		if err := identity.Validate(sessionID, "session"); err != nil {
			return false, err
		}
		err := store.withSessionFileLock(sessionID, func() error {
			journal, err := store.readTrashMoveJournal(sessionID)
			if errors.Is(err, os.ErrNotExist) {
				// Another Store may have completed the same journal after
				// this directory snapshot was taken.
				return nil
			}
			if err != nil {
				return err
			}
			return store.finishTrashMove(journal)
		})
		if err != nil && !errors.Is(err, errSessionIndexNotReady) {
			return false, err
		}
		recovered = true
	}
	return recovered, nil
}

func (store *Store) finishTrashMove(
	journal sessionTrashMoveJournal,
) error {
	if err := store.reconcileTrashMove(journal); err != nil {
		return err
	}
	if err := store.removeSessionFromIndex(journal.SessionID); err != nil {
		return fmt.Errorf(
			"remove Session %s from index: %w", journal.SessionID, err,
		)
	}
	return store.removeTrashMoveJournal(journal)
}

func (store *Store) completeTrashMove(sessionID string) error {
	journal, err := store.readTrashMoveJournal(sessionID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return store.removeTrashMoveJournal(journal)
}

func (store *Store) completeAllTrashMoves() error {
	directory, err := store.openPinnedDirectory(store.moveDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	entries, err := directory.entries()
	closeErr := directory.close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range entries {
		if entry.isRegular() && isOwnedAtomicTempName(entry.name) {
			continue
		}
		if !entry.isRegular() || filepath.Ext(entry.name) != ".json" {
			return fmt.Errorf(
				"Session trash move journal contains unsupported entry %q",
				entry.name,
			)
		}
		sessionID := strings.TrimSuffix(entry.name, ".json")
		if err := store.completeTrashMove(sessionID); err != nil {
			return err
		}
	}
	return nil
}

func validateTrashMoveJournal(journal sessionTrashMoveJournal) error {
	if journal.Version != sessionTrashMoveVersion {
		return fmt.Errorf(
			"unsupported Session trash move version %d", journal.Version,
		)
	}
	if err := identity.Validate(journal.SessionID, "session"); err != nil {
		return err
	}
	if journal.RootDevice == 0 || journal.RootInode == 0 {
		return fmt.Errorf("Session trash move has no source identity")
	}
	if filepath.IsAbs(journal.TargetRelative) ||
		filepath.Clean(journal.TargetRelative) != journal.TargetRelative {
		return fmt.Errorf("unsafe Session trash target %q", journal.TargetRelative)
	}
	parts := strings.Split(
		journal.TargetRelative, string(filepath.Separator),
	)
	if len(parts) != 4 ||
		parts[0] != "_system" ||
		parts[1] != "trash" ||
		parts[2] == "" ||
		parts[2] == "." ||
		parts[2] == ".." ||
		parts[3] != journal.SessionID {
		return fmt.Errorf(
			"unsupported Session trash target %q", journal.TargetRelative,
		)
	}
	return nil
}

func (store *Store) persistTrashMoveJournal(
	journal sessionTrashMoveJournal,
) error {
	if err := validateTrashMoveJournal(journal); err != nil {
		return err
	}
	if err := store.ensure(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	directory, err := store.openPinnedDirectory(store.moveDir)
	if err != nil {
		return err
	}
	defer directory.close()
	if _, err := directory.statEntry(journal.SessionID + ".json"); err == nil {
		current, readErr := store.readTrashMoveJournal(journal.SessionID)
		if readErr != nil {
			return readErr
		}
		if current != journal {
			return fmt.Errorf(
				"Session %s already has a different pending trash move",
				journal.SessionID,
			)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return directory.atomicBytes(
		journal.SessionID+".json", data, 0o600, nil,
	)
}

func (store *Store) readTrashMoveJournal(
	sessionID string,
) (sessionTrashMoveJournal, error) {
	directory, err := store.openPinnedDirectory(store.moveDir)
	if err != nil {
		return sessionTrashMoveJournal{}, err
	}
	defer directory.close()
	var journal sessionTrashMoveJournal
	if err := directory.readStrictJSON(
		sessionID+".json", maxTrashMoveBytes, &journal,
	); err != nil {
		return sessionTrashMoveJournal{}, err
	}
	if journal.SessionID != sessionID {
		return sessionTrashMoveJournal{}, fmt.Errorf(
			"Session trash move identity does not match its filename",
		)
	}
	if err := validateTrashMoveJournal(journal); err != nil {
		return sessionTrashMoveJournal{}, err
	}
	return journal, nil
}

func (store *Store) removeTrashMoveJournal(
	journal sessionTrashMoveJournal,
) error {
	directory, err := store.openPinnedDirectory(store.moveDir)
	if err != nil {
		return err
	}
	defer directory.close()
	entry, err := directory.statEntry(journal.SessionID + ".json")
	if err != nil {
		return err
	}
	if !entry.isRegular() || entry.nlink != 1 {
		return fmt.Errorf("Session trash move journal is not a regular file")
	}
	journalIdentity := entry.identity()
	return directory.removeRegular(
		journal.SessionID+".json", &journalIdentity,
	)
}

func (store *Store) reconcileTrashMove(
	journal sessionTrashMoveJournal,
) error {
	if err := validateTrashMoveJournal(journal); err != nil {
		return err
	}
	sessions, err := store.openSessionsDirectory()
	if err != nil {
		return err
	}
	defer sessions.close()
	parts := strings.Split(
		journal.TargetRelative, string(filepath.Separator),
	)
	trash, err := store.openPinnedDirectory(
		filepath.Join(store.historyDir, "trash"),
	)
	if err != nil {
		return err
	}
	defer trash.close()
	targetName := parts[3]
	targetParent, err := trash.openDirectory(parts[2], true)
	if err != nil {
		return err
	}
	defer targetParent.close()
	if err := store.pinOpenedDirectory(targetParent); err != nil {
		return err
	}

	source, sourceErr := sessions.statEntry(journal.SessionID)
	target, targetErr := targetParent.statEntry(targetName)
	sourceMissing := errors.Is(sourceErr, os.ErrNotExist)
	targetMissing := errors.Is(targetErr, os.ErrNotExist)
	if sourceErr != nil && !sourceMissing {
		return sourceErr
	}
	if targetErr != nil && !targetMissing {
		return targetErr
	}
	expected := safeFileIdentity{
		dev: journal.RootDevice,
		ino: journal.RootInode,
	}
	renamed := false
	switch {
	case !sourceMissing && targetMissing:
		if !source.isDirectory() || !source.sameIdentity(expected) {
			return fmt.Errorf(
				"Session trash source does not match its durable identity",
			)
		}
		store.hitMutationFailpoint(
			"before_trash_rename", journal.TargetRelative,
		)
		if err := store.hitMutationErrorpoint(
			"before_trash_rename", journal.TargetRelative,
		); err != nil {
			return err
		}
		if err := renameNoReplaceAt(
			sessions.fd, journal.SessionID,
			targetParent.fd, targetName,
		); err != nil {
			return err
		}
		visible, err := targetParent.statEntry(targetName)
		if err != nil {
			return err
		}
		if !visible.isDirectory() || !visible.sameIdentity(expected) {
			restoreErr := renameNoReplaceAt(
				targetParent.fd, targetName,
				sessions.fd, journal.SessionID,
			)
			syncErr := errors.Join(
				targetParent.sync(), sessions.sync(),
			)
			return errors.Join(
				fmt.Errorf(
					"Session trash source changed identity at publication",
				),
				restoreErr, syncErr,
			)
		}
		renamed = true
	case sourceMissing && !targetMissing:
		if !target.isDirectory() || !target.sameIdentity(expected) {
			return fmt.Errorf(
				"Session trash target does not match its durable identity",
			)
		}
	case !sourceMissing && !targetMissing:
		return fmt.Errorf("Session trash source and target both exist")
	default:
		return fmt.Errorf("Session trash source and target are both missing")
	}
	if renamed {
		if err := store.hitMutationErrorpoint(
			"after_trash_rename_before_sync", journal.TargetRelative,
		); err != nil {
			return err
		}
	}
	if err := targetParent.sync(); err != nil {
		return fmt.Errorf("persist Session trash target: %w", err)
	}
	if err := store.hitMutationErrorpoint(
		"after_trash_target_sync", journal.TargetRelative,
	); err != nil {
		return err
	}
	if err := sessions.sync(); err != nil {
		return fmt.Errorf("persist Session source removal: %w", err)
	}
	store.forgetDirectoryIdentity(store.sessionDir(journal.SessionID))
	return nil
}
