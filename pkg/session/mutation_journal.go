package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/yy003x/runtime/internal/domain/identity"
)

const (
	sessionMutationVersion  = 3
	maxMutationJournalBytes = 32 << 20
	maxMutationOwnerBytes   = 4 << 10
	mutationOwnerFileName   = ".runtime-mutation-owner.json"

	mutationPrepared  = "prepared"
	mutationCommitted = "committed"

	mutationReplace = "replace"
	mutationAppend  = "append"
)

type sessionMutationJournal struct {
	MutationVersion int                     `json:"mutation_version"`
	SessionID       string                  `json:"session_id"`
	Nonce           string                  `json:"nonce"`
	State           string                  `json:"state"`
	SessionExisted  bool                    `json:"session_existed"`
	RootDevice      uint64                  `json:"root_device,omitempty"`
	RootInode       uint64                  `json:"root_inode,omitempty"`
	Entries         []sessionMutationBackup `json:"entries"`
}

type sessionMutationOwner struct {
	MutationVersion int    `json:"mutation_version"`
	SessionID       string `json:"session_id"`
	Nonce           string `json:"nonce"`
	RootDevice      uint64 `json:"root_device"`
	RootInode       uint64 `json:"root_inode"`
}

type mutationFileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func persistentMutationIdentity(
	identity safeFileIdentity,
) mutationFileIdentity {
	return mutationFileIdentity{
		Device: identity.dev,
		Inode:  identity.ino,
	}
}

func (identity mutationFileIdentity) safeIdentity() safeFileIdentity {
	return safeFileIdentity{dev: identity.Device, ino: identity.Inode}
}

type sessionMutationBackup struct {
	RelativePath string                 `json:"relative_path"`
	Kind         string                 `json:"kind"`
	Existed      bool                   `json:"existed"`
	Device       uint64                 `json:"device,omitempty"`
	Inode        uint64                 `json:"inode,omitempty"`
	Owned        []mutationFileIdentity `json:"owned_identities,omitempty"`
	Data         []byte                 `json:"data,omitempty"`
	Size         int64                  `json:"size,omitempty"`
	PrefixDigest string                 `json:"prefix_digest,omitempty"`
}

type sessionMutation struct {
	journal sessionMutationJournal
	backed  map[string]struct{}
	written map[string]mutationFileIdentity
	dirty   bool
	owned   bool
}

func (store *Store) recoverExistingMutations() error {
	journalDirectory, err := store.openPinnedDirectory(store.journalDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer journalDirectory.close()
	entries, err := journalDirectory.entries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.isRegular() && isOwnedAtomicTempName(entry.name) {
			continue
		}
		if !entry.isRegular() ||
			filepath.Ext(entry.name) != ".json" {
			return fmt.Errorf(
				"Session mutation journal contains unsupported entry %q",
				entry.name,
			)
		}
		sessionID := strings.TrimSuffix(entry.name, ".json")
		if err := identity.Validate(sessionID, "session"); err != nil {
			return fmt.Errorf(
				"Session mutation journal contains invalid entry %q: %w",
				entry.name, err,
			)
		}
		if err := store.withSessionFileLock(sessionID, func() error {
			return store.recoverMutationLocked(sessionID)
		}); err != nil && !errors.Is(err, errSessionIndexNotReady) {
			return err
		}
	}
	return nil
}

func (store *Store) beginMutation(sessionID string) error {
	if err := store.recoverMutationLocked(sessionID); err != nil {
		return err
	}
	nonce, err := newMutationNonce()
	if err != nil {
		return err
	}
	sessionExisted := false
	var rootIdentity safeFileIdentity
	root, err := store.openSessionRoot(sessionID)
	switch {
	case err == nil:
		rootIdentity, err = root.identity()
		if err != nil {
			root.close()
			return err
		}
		if closeErr := root.close(); closeErr != nil {
			return closeErr
		}
		if err := store.requireNoMutationOwner(sessionID); err != nil {
			return err
		}
		sessionExisted = true
	case errors.Is(err, os.ErrNotExist):
	default:
		return err
	}
	store.mutationMu.Lock()
	defer store.mutationMu.Unlock()
	if _, exists := store.activeMutations[sessionID]; exists {
		return fmt.Errorf("Session %s already has an active mutation", sessionID)
	}
	store.activeMutations[sessionID] = &sessionMutation{
		journal: sessionMutationJournal{
			MutationVersion: sessionMutationVersion,
			SessionID:       sessionID,
			Nonce:           nonce,
			State:           mutationPrepared,
			SessionExisted:  sessionExisted,
			RootDevice:      rootIdentity.dev,
			RootInode:       rootIdentity.ino,
		},
		backed:  make(map[string]struct{}),
		written: make(map[string]mutationFileIdentity),
	}
	return nil
}

func (store *Store) finishMutation(sessionID string, commit bool) error {
	store.mutationMu.Lock()
	mutation := store.activeMutations[sessionID]
	store.mutationMu.Unlock()
	if mutation == nil {
		return fmt.Errorf("Session %s has no active mutation", sessionID)
	}
	defer func() {
		store.mutationMu.Lock()
		delete(store.activeMutations, sessionID)
		store.mutationMu.Unlock()
	}()
	if !mutation.dirty {
		return nil
	}
	if !commit {
		if err := store.rollbackMutation(mutation.journal); err != nil {
			return err
		}
		return store.removeMutationJournal(sessionID)
	}
	if err := store.hitMutationErrorpoint("before_commit_marker", ""); err != nil {
		rollbackErr := store.rollbackMutation(mutation.journal)
		if rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		return errors.Join(err, store.removeMutationJournal(sessionID))
	}
	prepared := mutation.journal
	committed := prepared
	committed.State = mutationCommitted
	if err := store.persistMutationJournal(committed); err != nil {
		return store.resolveCommitPersistError(prepared, err)
	}
	mutation.journal = committed
	store.hitMutationFailpoint("after_commit_marker", "")
	if err := store.hitMutationErrorpoint("before_journal_cleanup", ""); err != nil {
		return err
	}
	return store.cleanupCommittedMutation(committed)
}

func (store *Store) resolveCommitPersistError(
	prepared sessionMutationJournal,
	persistErr error,
) error {
	disk, err := store.readMutationJournal(prepared.SessionID)
	if err != nil {
		return errors.Join(
			persistErr,
			fmt.Errorf(
				"Session mutation commit is ambiguous; preserve facts for "+
					"manual recovery: %w",
				err,
			),
		)
	}
	if err := store.validateMutationJournal(prepared.SessionID, disk); err != nil {
		return errors.Join(
			persistErr,
			fmt.Errorf(
				"Session mutation commit is ambiguous; on-disk journal "+
					"failed validation: %w",
				err,
			),
		)
	}
	if !sameMutationIdentity(prepared, disk) {
		return errors.Join(
			persistErr,
			fmt.Errorf(
				"Session mutation commit is ambiguous; on-disk journal "+
					"identity changed",
			),
		)
	}
	switch disk.State {
	case mutationCommitted:
		// The rename succeeded and only the durability acknowledgement failed.
		// Retry the directory barrier, then finish committed cleanup. Facts
		// must never be rolled back after a committed journal is observable.
		journalDirectory, err := store.openPinnedDirectory(store.journalDir)
		if err != nil {
			return errors.Join(persistErr, err)
		}
		syncErr := journalDirectory.sync()
		closeErr := journalDirectory.close()
		if syncErr != nil || closeErr != nil {
			err := errors.Join(syncErr, closeErr)
			return errors.Join(persistErr, err)
		}
		if err := store.cleanupCommittedMutation(disk); err != nil {
			return errors.Join(persistErr, err)
		}
		return nil
	case mutationPrepared:
		if err := store.rollbackMutation(disk); err != nil {
			return errors.Join(persistErr, err)
		}
		return errors.Join(
			persistErr,
			store.removeMutationJournal(prepared.SessionID),
		)
	default:
		return errors.Join(
			persistErr,
			fmt.Errorf(
				"Session mutation commit is ambiguous; on-disk state=%q",
				disk.State,
			),
		)
	}
}

func sameMutationIdentity(
	left, right sessionMutationJournal,
) bool {
	left.State = ""
	right.State = ""
	return reflect.DeepEqual(left, right)
}

func (store *Store) prepareReplace(
	sessionID, path string,
) (string, error) {
	return store.prepareMutationTarget(sessionID, path, mutationReplace)
}

func (store *Store) prepareAppend(
	sessionID, path string,
) (string, error) {
	return store.prepareMutationTarget(sessionID, path, mutationAppend)
}

func (store *Store) prepareMutationTarget(
	sessionID, path, kind string,
) (string, error) {
	mutation, err := store.activeMutation(sessionID)
	if err != nil {
		return "", err
	}
	relativePath, err := store.mutationRelativePath(sessionID, path, kind)
	if err != nil {
		return "", err
	}
	if _, exists := mutation.backed[relativePath]; exists {
		return relativePath, nil
	}
	backup := sessionMutationBackup{
		RelativePath: relativePath,
		Kind:         kind,
	}
	store.hitMutationFailpoint("before_mutation_backup", relativePath)
	root, openErr := store.openSessionRoot(sessionID)
	switch {
	case openErr == nil:
		defer root.close()
		rootIdentity, identityErr := root.identity()
		if identityErr != nil {
			return "", identityErr
		}
		if mutation.journal.RootDevice != 0 &&
			(rootIdentity.dev != mutation.journal.RootDevice ||
				rootIdentity.ino != mutation.journal.RootInode) {
			return "", fmt.Errorf(
				"Session root %s changed during mutation", sessionID,
			)
		}
		limit := int64(maxFactLineBytes)
		if kind == mutationAppend {
			limit = maxFactFileBytes
		}
		data, fileEntry, readErr := root.readRegularFact(relativePath, limit)
		if readErr == nil {
			backup.Existed = true
			backup.Device = fileEntry.dev
			backup.Inode = fileEntry.ino
			if kind == mutationAppend {
				backup.Size = int64(len(data))
				digest := sha256.Sum256(data)
				backup.PrefixDigest = "sha256:" + hex.EncodeToString(digest[:])
			} else {
				backup.Data = data
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return "", readErr
		}
	case errors.Is(openErr, os.ErrNotExist):
	default:
		return "", openErr
	}
	if backup.Existed {
		switch kind {
		case mutationAppend:
			if backup.Size > maxFactFileBytes {
				return "", fmt.Errorf(
					"%s exceeds %d bytes", path, maxFactFileBytes,
				)
			}
		case mutationReplace:
			if len(backup.Data) > maxFactLineBytes {
				return "", fmt.Errorf(
					"%s exceeds %d bytes", path, maxFactLineBytes,
				)
			}
		}
	}
	mutation.journal.Entries = append(mutation.journal.Entries, backup)
	mutation.backed[relativePath] = struct{}{}
	mutation.dirty = true
	if err := store.persistMutationJournal(mutation.journal); err != nil {
		return "", err
	}
	if !mutation.journal.SessionExisted && !mutation.owned {
		if err := store.createMutationOwner(mutation.journal); err != nil {
			return "", err
		}
		mutation.owned = true
	}
	store.hitMutationFailpoint("after_journal_prepare", relativePath)
	return relativePath, nil
}

func (store *Store) activeMutation(
	sessionID string,
) (*sessionMutation, error) {
	store.mutationMu.Lock()
	defer store.mutationMu.Unlock()
	mutation := store.activeMutations[sessionID]
	if mutation == nil {
		return nil, fmt.Errorf(
			"Session fact mutation for %s requires the Session lock",
			sessionID,
		)
	}
	return mutation, nil
}

func (store *Store) persistMutationJournal(
	value sessionMutationJournal,
) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxMutationJournalBytes {
		return fmt.Errorf(
			"Session mutation journal for %s exceeds %d bytes",
			value.SessionID, maxMutationJournalBytes,
		)
	}
	beforeRenameStage := "before_" + value.State + "_journal_rename"
	store.hitMutationFailpoint(beforeRenameStage, "")
	if err := store.hitMutationErrorpoint(beforeRenameStage, ""); err != nil {
		return err
	}
	if err := store.ensure(); err != nil {
		return err
	}
	journalDirectory, err := store.openPinnedDirectory(store.journalDir)
	if err != nil {
		return err
	}
	defer journalDirectory.close()
	return journalDirectory.atomicBytes(
		value.SessionID+".json", data, 0o600,
		func() error {
			stage := "after_" + value.State +
				"_journal_rename_before_directory_sync"
			store.hitMutationFailpoint(stage, "")
			return store.hitMutationErrorpoint(stage, "")
		},
	)
}

func (store *Store) readMutationJournal(
	sessionID string,
) (sessionMutationJournal, error) {
	var journal sessionMutationJournal
	journalDirectory, err := store.openPinnedDirectory(store.journalDir)
	if err != nil {
		return sessionMutationJournal{}, err
	}
	defer journalDirectory.close()
	if err := journalDirectory.readStrictJSON(
		sessionID+".json", maxMutationJournalBytes, &journal,
	); err != nil {
		return sessionMutationJournal{}, err
	}
	return journal, nil
}

func (store *Store) recoverMutationLocked(sessionID string) error {
	path := store.mutationJournalPath(sessionID)
	journalDirectory, err := store.openPinnedDirectory(store.journalDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer journalDirectory.close()
	_, err = journalDirectory.statEntry(sessionID + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	journal, err := store.readMutationJournal(sessionID)
	if err != nil {
		return fmt.Errorf("decode Session mutation journal %s: %w", path, err)
	}
	if err := store.validateMutationJournal(sessionID, journal); err != nil {
		return fmt.Errorf("validate Session mutation journal %s: %w", path, err)
	}
	switch journal.State {
	case mutationPrepared:
		if err := store.rollbackMutation(journal); err != nil {
			return err
		}
	case mutationCommitted:
		return store.cleanupCommittedMutation(journal)
	default:
		return fmt.Errorf(
			"Session mutation journal %s has unsupported state %q",
			path, journal.State,
		)
	}
	return store.removeMutationJournal(sessionID)
}

func (store *Store) validateMutationJournal(
	sessionID string,
	journal sessionMutationJournal,
) error {
	if journal.MutationVersion != sessionMutationVersion {
		return fmt.Errorf(
			"unsupported mutation_version %d; expected %d",
			journal.MutationVersion, sessionMutationVersion,
		)
	}
	if journal.SessionID != sessionID {
		return fmt.Errorf(
			"session_id=%q does not match journal name",
			journal.SessionID,
		)
	}
	if err := validateMutationNonce(journal.Nonce); err != nil {
		return err
	}
	switch journal.State {
	case mutationPrepared, mutationCommitted:
	default:
		return fmt.Errorf("unsupported mutation state %q", journal.State)
	}
	if len(journal.Entries) == 0 {
		return fmt.Errorf("mutation journal has no targets")
	}
	seen := make(map[string]struct{}, len(journal.Entries))
	for _, entry := range journal.Entries {
		if _, exists := seen[entry.RelativePath]; exists {
			return fmt.Errorf(
				"duplicate mutation target %q", entry.RelativePath,
			)
		}
		seen[entry.RelativePath] = struct{}{}
		if !journal.SessionExisted && entry.Existed {
			return fmt.Errorf(
				"new Session mutation target %q has a preimage",
				entry.RelativePath,
			)
		}
		if entry.Existed {
			if entry.Device == 0 || entry.Inode == 0 {
				return fmt.Errorf(
					"existing mutation target %q has no file identity",
					entry.RelativePath,
				)
			}
		} else if entry.Device != 0 || entry.Inode != 0 {
			return fmt.Errorf(
				"new mutation target %q has a preimage identity",
				entry.RelativePath,
			)
		}
		owned := make(map[mutationFileIdentity]struct{}, len(entry.Owned))
		for _, identity := range entry.Owned {
			if identity.Device == 0 || identity.Inode == 0 {
				return fmt.Errorf(
					"mutation target %q has an invalid owned identity",
					entry.RelativePath,
				)
			}
			if _, duplicate := owned[identity]; duplicate {
				return fmt.Errorf(
					"mutation target %q repeats an owned identity",
					entry.RelativePath,
				)
			}
			owned[identity] = struct{}{}
		}
		if _, err := store.mutationRelativePath(
			sessionID,
			filepath.Join(store.sessionDir(sessionID), entry.RelativePath),
			entry.Kind,
		); err != nil {
			return err
		}
		switch entry.Kind {
		case mutationReplace:
			if entry.Size != 0 {
				return fmt.Errorf(
					"replace target %q has an append size",
					entry.RelativePath,
				)
			}
			if len(entry.Data) > maxFactLineBytes {
				return fmt.Errorf(
					"replace target %q exceeds %d bytes",
					entry.RelativePath, maxFactLineBytes,
				)
			}
			if !entry.Existed && len(entry.Data) != 0 {
				return fmt.Errorf(
					"new replace target %q has a preimage",
					entry.RelativePath,
				)
			}
		case mutationAppend:
			if len(entry.Data) != 0 {
				return fmt.Errorf(
					"append target %q has a replace preimage",
					entry.RelativePath,
				)
			}
			if entry.Size < 0 || entry.Size > maxFactFileBytes {
				return fmt.Errorf(
					"append target %q has invalid size %d",
					entry.RelativePath, entry.Size,
				)
			}
			if !entry.Existed && entry.Size != 0 {
				return fmt.Errorf(
					"new append target %q has a preexisting size",
					entry.RelativePath,
				)
			}
			if entry.Existed {
				if len(entry.PrefixDigest) != len("sha256:")+sha256.Size*2 ||
					!strings.HasPrefix(entry.PrefixDigest, "sha256:") {
					return fmt.Errorf(
						"append target %q has invalid prefix digest",
						entry.RelativePath,
					)
				}
				if _, err := hex.DecodeString(
					strings.TrimPrefix(entry.PrefixDigest, "sha256:"),
				); err != nil {
					return fmt.Errorf(
						"append target %q has invalid prefix digest",
						entry.RelativePath,
					)
				}
			} else if entry.PrefixDigest != "" {
				return fmt.Errorf(
					"new append target %q has a prefix digest",
					entry.RelativePath,
				)
			}
		default:
			return fmt.Errorf(
				"unsupported mutation kind %q", entry.Kind,
			)
		}
	}
	if (journal.RootDevice == 0) != (journal.RootInode == 0) {
		return fmt.Errorf("mutation journal has incomplete Session root identity")
	}
	if journal.SessionExisted && journal.RootDevice == 0 {
		return fmt.Errorf("existing Session mutation has no root identity")
	}
	return nil
}

func newMutationNonce() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate Session mutation nonce: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func validateMutationNonce(value string) error {
	if len(value) != 32 || strings.ToLower(value) != value {
		return fmt.Errorf("Session mutation nonce must be 32 lowercase hex characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 16 {
		return fmt.Errorf("Session mutation nonce must be 32 lowercase hex characters")
	}
	return nil
}

func (store *Store) mutationOwnerPath(sessionID string) string {
	return filepath.Join(store.sessionDir(sessionID), mutationOwnerFileName)
}

func mutationOwnerFor(
	journal sessionMutationJournal,
) sessionMutationOwner {
	return sessionMutationOwner{
		MutationVersion: journal.MutationVersion,
		SessionID:       journal.SessionID,
		Nonce:           journal.Nonce,
		RootDevice:      journal.RootDevice,
		RootInode:       journal.RootInode,
	}
}

func (store *Store) createMutationOwner(
	journal sessionMutationJournal,
) error {
	root := store.sessionDir(journal.SessionID)
	createdRoot, tempName, err := store.createSessionRoot(
		journal.SessionID, journal.Nonce,
	)
	if err != nil {
		return fmt.Errorf("create new Session mutation root %s: %w", root, err)
	}
	rootIdentity, err := createdRoot.identity()
	if err != nil {
		createdRoot.close()
		return err
	}
	journal.RootDevice = rootIdentity.dev
	journal.RootInode = rootIdentity.ino
	data, err := json.MarshalIndent(mutationOwnerFor(journal), "", "  ")
	if err != nil {
		createdRoot.close()
		return err
	}
	data = append(data, '\n')
	if _, err := createdRoot.atomicWrite(
		mutationOwnerFileName, data, 0o600, true, nil, nil, nil, nil,
	); err != nil {
		createdRoot.close()
		return fmt.Errorf("persist new Session mutation owner: %w", err)
	}
	if err := store.persistMutationJournal(journal); err != nil {
		createdRoot.close()
		return fmt.Errorf("persist new Session root identity: %w", err)
	}
	store.mutationMu.Lock()
	mutation := store.activeMutations[journal.SessionID]
	if mutation != nil {
		mutation.journal = journal
	}
	store.mutationMu.Unlock()
	if mutation == nil {
		createdRoot.close()
		return fmt.Errorf(
			"Session %s has no active mutation", journal.SessionID,
		)
	}
	if err := createdRoot.close(); err != nil {
		return err
	}
	if err := store.publishSessionRoot(
		journal.SessionID, tempName, rootIdentity,
	); err != nil {
		return fmt.Errorf("publish new Session mutation root %s: %w", root, err)
	}
	store.hitMutationFailpoint("after_mutation_root_create", "")
	store.hitMutationFailpoint("after_mutation_owner_write", "")
	return nil
}

func (store *Store) readMutationOwner(
	sessionID string,
) (sessionMutationOwner, error) {
	var owner sessionMutationOwner
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return sessionMutationOwner{}, err
	}
	defer root.close()
	if err := root.readStrictJSON(
		mutationOwnerFileName, maxMutationOwnerBytes, &owner,
	); err != nil {
		return sessionMutationOwner{}, err
	}
	return owner, nil
}

func validateMutationOwner(
	journal sessionMutationJournal,
	owner sessionMutationOwner,
) error {
	expected := mutationOwnerFor(journal)
	if owner != expected {
		return fmt.Errorf(
			"Session mutation owner does not match journal nonce and identity",
		)
	}
	return nil
}

func validateMutationRootIdentity(
	journal sessionMutationJournal,
	root *safeDirectory,
) error {
	if journal.RootDevice == 0 || journal.RootInode == 0 {
		return fmt.Errorf(
			"Session mutation for %s has no durable root identity",
			journal.SessionID,
		)
	}
	identity, err := root.identity()
	if err != nil {
		return err
	}
	if identity.dev != journal.RootDevice ||
		identity.ino != journal.RootInode {
		return fmt.Errorf(
			"Session root %s does not match its mutation identity",
			journal.SessionID,
		)
	}
	return nil
}

func (store *Store) openOrPublishMutationRoot(
	journal sessionMutationJournal,
) (*safeDirectory, error) {
	root, err := store.openSessionRoot(journal.SessionID)
	if err == nil ||
		!errors.Is(err, os.ErrNotExist) ||
		journal.SessionExisted ||
		journal.RootDevice == 0 ||
		journal.RootInode == 0 {
		return root, err
	}
	sessions, err := store.openSessionsDirectory()
	if err != nil {
		return nil, err
	}
	tempName := mutationRootTempName(journal.Nonce)
	tempRoot, err := sessions.openDirectory(tempName, false)
	closeErr := sessions.close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		tempRoot.close()
		return nil, closeErr
	}
	if err := store.pinOpenedDirectory(tempRoot); err != nil {
		tempRoot.close()
		return nil, err
	}
	if err := validateMutationRootIdentity(journal, tempRoot); err != nil {
		tempRoot.close()
		return nil, err
	}
	var owner sessionMutationOwner
	if err := tempRoot.readStrictJSON(
		mutationOwnerFileName, maxMutationOwnerBytes, &owner,
	); err != nil {
		tempRoot.close()
		return nil, err
	}
	if err := validateMutationOwner(journal, owner); err != nil {
		tempRoot.close()
		return nil, err
	}
	expected, err := tempRoot.identity()
	if err != nil {
		tempRoot.close()
		return nil, err
	}
	if err := tempRoot.close(); err != nil {
		return nil, err
	}
	if err := store.publishSessionRoot(
		journal.SessionID, tempName, expected,
	); err != nil {
		return nil, err
	}
	return store.openSessionRoot(journal.SessionID)
}

func (store *Store) requireNoMutationOwner(sessionID string) error {
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return err
	}
	defer root.close()
	if _, err := root.statEntry(mutationOwnerFileName); errors.Is(
		err, os.ErrNotExist,
	) {
		return nil
	} else if err != nil {
		return err
	}
	return fmt.Errorf(
		"existing Session %s contains an unowned mutation marker",
		sessionID,
	)
}

func (store *Store) cleanupCommittedMutation(
	journal sessionMutationJournal,
) error {
	if err := store.updateIndexForCommittedSession(journal.SessionID); err != nil {
		return fmt.Errorf(
			"update Session index for committed mutation %s: %w",
			journal.SessionID, err,
		)
	}
	if journal.SessionExisted {
		if err := store.requireNoMutationOwner(journal.SessionID); err != nil {
			return err
		}
	} else if err := store.removeMutationOwner(journal, true); err != nil {
		return err
	} else {
		store.hitMutationFailpoint("after_mutation_owner_cleanup", "")
	}
	return store.removeMutationJournal(journal.SessionID)
}

func (store *Store) removeMutationOwner(
	journal sessionMutationJournal,
	allowMissing bool,
) error {
	root, err := store.openSessionRoot(journal.SessionID)
	if err != nil {
		if allowMissing && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer root.close()
	if err := validateMutationRootIdentity(journal, root); err != nil {
		return err
	}
	if _, err := root.statEntry(mutationOwnerFileName); errors.Is(
		err, os.ErrNotExist,
	) {
		if allowMissing {
			return nil
		}
		return fmt.Errorf("new Session mutation owner is missing")
	} else if err != nil {
		return err
	}
	var owner sessionMutationOwner
	ownerEntry, err := root.readStrictJSONFact(
		mutationOwnerFileName, maxMutationOwnerBytes, &owner,
	)
	if err != nil {
		return fmt.Errorf("read new Session mutation owner: %w", err)
	}
	if err := validateMutationOwner(journal, owner); err != nil {
		return err
	}
	ownerIdentity := ownerEntry.identity()
	if err := root.removeRegular(
		mutationOwnerFileName, &ownerIdentity,
	); err != nil {
		return err
	}
	return nil
}

func (store *Store) rollbackMutation(
	journal sessionMutationJournal,
) error {
	var scope newSessionRollbackScope
	if !journal.SessionExisted {
		var err error
		scope, err = store.validateNewSessionRollbackScope(journal)
		if err != nil {
			return err
		}
		if err := store.removeNewSessionOwnedTemps(
			journal.SessionID, scope.ownedTemps,
		); err != nil {
			return err
		}
		if scope.ownerless {
			return store.removeNewSessionRoot(journal)
		}
	} else if err := store.requireNoMutationOwner(journal.SessionID); err != nil {
		return err
	}
	store.hitMutationFailpoint("before_rollback_root_open", "")
	root, err := store.openSessionRoot(journal.SessionID)
	if errors.Is(err, os.ErrNotExist) && !journal.SessionExisted {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.close()
	if err := validateMutationRootIdentity(journal, root); err != nil {
		return err
	}
	for index := len(journal.Entries) - 1; index >= 0; index-- {
		entry := &journal.Entries[index]
		store.hitMutationFailpoint(
			"before_rollback_target_open", entry.RelativePath,
		)
		current, statErr := root.stat(entry.RelativePath)
		missing := errors.Is(statErr, os.ErrNotExist)
		if statErr != nil && !missing {
			return statErr
		}
		var currentIdentity safeFileIdentity
		if !missing {
			if !current.isRegular() || current.nlink != 1 {
				return fmt.Errorf(
					"Session mutation target %q is not a single-link regular file",
					entry.RelativePath,
				)
			}
			currentIdentity = current.identity()
			if !mutationBackupOwnsIdentity(*entry, currentIdentity) {
				return fmt.Errorf(
					"Session mutation target %q does not match its durable identity",
					entry.RelativePath,
				)
			}
		}
		switch entry.Kind {
		case mutationReplace:
			if entry.Existed {
				if missing {
					return fmt.Errorf(
						"existing Session mutation target %q is missing",
						entry.RelativePath,
					)
				}
				_, err := root.atomicWrite(
					entry.RelativePath,
					entry.Data,
					0o600,
					false,
					&currentIdentity,
					func(identity safeFileIdentity) error {
						return store.recordRollbackOwnedIdentity(
							&journal, index, identity,
						)
					},
					nil,
					nil,
				)
				if err != nil {
					return err
				}
			} else if !missing {
				if err := root.removeRegular(
					entry.RelativePath, &currentIdentity,
				); err != nil {
					return err
				}
			}
		case mutationAppend:
			if entry.Existed {
				if missing {
					return fmt.Errorf(
						"existing Session append target %q is missing",
						entry.RelativePath,
					)
				}
				preimage := safeFileIdentity{
					dev: entry.Device,
					ino: entry.Inode,
				}
				data, openedEntry, err := root.readRegularFact(
					entry.RelativePath, maxFactFileBytes,
				)
				if err != nil {
					return err
				}
				if !openedEntry.sameIdentity(currentIdentity) {
					return fmt.Errorf(
						"Session append target %q changed during rollback",
						entry.RelativePath,
					)
				}
				if int64(len(data)) < entry.Size {
					return fmt.Errorf(
						"Session append target %q is shorter than its preimage",
						entry.RelativePath,
					)
				}
				prefix := data[:entry.Size]
				digest := sha256.Sum256(prefix)
				if "sha256:"+hex.EncodeToString(digest[:]) !=
					entry.PrefixDigest {
					return fmt.Errorf(
						"Session append target %q prefix does not match its backup",
						entry.RelativePath,
					)
				}
				if currentIdentity == preimage &&
					int64(len(data)) == entry.Size {
					break
				}
				_, err = root.atomicWrite(
					entry.RelativePath,
					prefix,
					0o600,
					false,
					&currentIdentity,
					func(identity safeFileIdentity) error {
						return store.recordRollbackOwnedIdentity(
							&journal, index, identity,
						)
					},
					nil,
					nil,
				)
				if err != nil {
					return err
				}
			} else if !missing {
				if err := root.removeRegular(
					entry.RelativePath, &currentIdentity,
				); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf(
				"unsupported mutation kind %q", entry.Kind,
			)
		}
		if err := store.hitMutationErrorpoint(
			"after_rollback_target", entry.RelativePath,
		); err != nil {
			return err
		}
		if !entry.Existed {
			if err := root.removeEmptyParents(entry.RelativePath); err != nil {
				return err
			}
		}
	}
	if !journal.SessionExisted {
		if err := store.removeMutationOwner(journal, false); err != nil {
			return err
		}
		return store.removeNewSessionRoot(journal)
	}
	return nil
}

func mutationBackupOwnsIdentity(
	entry sessionMutationBackup,
	identity safeFileIdentity,
) bool {
	if entry.Existed &&
		entry.Device == identity.dev &&
		entry.Inode == identity.ino {
		return true
	}
	persistent := persistentMutationIdentity(identity)
	for _, owned := range entry.Owned {
		if owned == persistent {
			return true
		}
	}
	return false
}

func (store *Store) recordRollbackOwnedIdentity(
	journal *sessionMutationJournal,
	entryIndex int,
	identity safeFileIdentity,
) error {
	persistent := persistentMutationIdentity(identity)
	for _, current := range journal.Entries[entryIndex].Owned {
		if current == persistent {
			return nil
		}
	}
	journal.Entries[entryIndex].Owned = append(
		journal.Entries[entryIndex].Owned, persistent,
	)
	return store.persistMutationJournal(*journal)
}

type newSessionRollbackScope struct {
	ownedTemps []ownedMutationTemp
	ownerless  bool
}

type ownedMutationTemp struct {
	relativePath string
	identity     safeFileIdentity
}

func (store *Store) validateNewSessionRollbackScope(
	journal sessionMutationJournal,
) (newSessionRollbackScope, error) {
	store.hitMutationFailpoint("before_new_session_scope_open", "")
	rootDirectory, err := store.openOrPublishMutationRoot(journal)
	if errors.Is(err, os.ErrNotExist) {
		sessions, openErr := store.openSessionsDirectory()
		if openErr != nil {
			return newSessionRollbackScope{}, openErr
		}
		_, tempErr := sessions.statEntry(mutationRootTempName(journal.Nonce))
		closeErr := sessions.close()
		if tempErr == nil {
			return newSessionRollbackScope{}, fmt.Errorf(
				"new Session mutation has an unowned temporary root",
			)
		}
		if !errors.Is(tempErr, os.ErrNotExist) {
			return newSessionRollbackScope{}, tempErr
		}
		if closeErr != nil {
			return newSessionRollbackScope{}, closeErr
		}
		return newSessionRollbackScope{ownerless: true}, nil
	}
	if err != nil {
		return newSessionRollbackScope{}, err
	}
	defer rootDirectory.close()

	if _, err := rootDirectory.statEntry(
		mutationOwnerFileName,
	); errors.Is(err, os.ErrNotExist) {
		ownedTemps, validateErr := validateOwnerlessNewSessionRoot(
			rootDirectory,
		)
		if validateErr != nil {
			return newSessionRollbackScope{}, fmt.Errorf(
				"new Session mutation owner is missing: %w",
				validateErr,
			)
		}
		if err := validateMutationRootIdentity(journal, rootDirectory); err != nil {
			return newSessionRollbackScope{}, err
		}
		return newSessionRollbackScope{
			ownedTemps: ownedTemps,
			ownerless:  true,
		}, nil
	} else if err != nil {
		return newSessionRollbackScope{}, err
	}
	var owner sessionMutationOwner
	if err := rootDirectory.readStrictJSON(
		mutationOwnerFileName, maxMutationOwnerBytes, &owner,
	); err != nil {
		return newSessionRollbackScope{}, fmt.Errorf(
			"read new Session mutation owner: %w", err,
		)
	}
	if err := validateMutationOwner(journal, owner); err != nil {
		return newSessionRollbackScope{}, err
	}
	if err := validateMutationRootIdentity(journal, rootDirectory); err != nil {
		return newSessionRollbackScope{}, err
	}

	allowedFiles := make(map[string]struct{}, len(journal.Entries))
	allowedDirectories := map[string]struct{}{".": {}}
	allowedTempDirectories := make(map[string]struct{}, len(journal.Entries))
	for _, entry := range journal.Entries {
		allowedFiles[entry.RelativePath] = struct{}{}
		if entry.Kind == mutationReplace {
			allowedTempDirectories[filepath.Dir(entry.RelativePath)] = struct{}{}
		}
		for current := filepath.Dir(entry.RelativePath); current != "."; {
			allowedDirectories[current] = struct{}{}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
	}
	var ownedTemps []ownedMutationTemp
	err = rootDirectory.walk(".", func(
		relativePath string,
		entry safeDirectoryEntry,
	) error {
		if entry.isSymlink() {
			return fmt.Errorf(
				"new Session rollback contains unregistered or unsafe content %q",
				relativePath,
			)
		}
		if entry.isDirectory() {
			if _, allowed := allowedDirectories[relativePath]; !allowed {
				return fmt.Errorf(
					"new Session rollback contains unregistered directory %q",
					relativePath,
				)
			}
			return nil
		}
		if relativePath == mutationOwnerFileName {
			if !entry.isRegular() {
				return fmt.Errorf(
					"new Session mutation owner is not a regular file",
				)
			}
			return nil
		}
		if entry.isRegular() && isOwnedAtomicTempName(entry.name) {
			tempDirectory := filepath.Dir(relativePath)
			if _, allowed := allowedTempDirectories[tempDirectory]; !allowed {
				return fmt.Errorf(
					"new Session rollback contains owned atomic temp "+
						"outside a replace target directory %q",
					relativePath,
				)
			}
			ownedTemps = append(ownedTemps, ownedMutationTemp{
				relativePath: relativePath,
				identity:     entry.identity(),
			})
			return nil
		}
		if _, allowed := allowedFiles[relativePath]; !allowed ||
			!entry.isRegular() {
			return fmt.Errorf(
				"new Session rollback contains unregistered or unsafe content %q",
				relativePath,
			)
		}
		return nil
	})
	if err != nil {
		return newSessionRollbackScope{}, err
	}
	return newSessionRollbackScope{ownedTemps: ownedTemps}, nil
}

func validateOwnerlessNewSessionRoot(
	root *safeDirectory,
) ([]ownedMutationTemp, error) {
	entries, err := root.entries()
	if err != nil {
		return nil, err
	}
	var ownedTemps []ownedMutationTemp
	for _, entry := range entries {
		if !entry.isRegular() || !isOwnedAtomicTempName(entry.name) {
			return nil, fmt.Errorf(
				"root contains content %q without a matching owner marker",
				entry.name,
			)
		}
		ownedTemps = append(ownedTemps, ownedMutationTemp{
			relativePath: entry.name,
			identity:     entry.identity(),
		})
	}
	return ownedTemps, nil
}

func (store *Store) removeNewSessionOwnedTemps(
	sessionID string,
	temps []ownedMutationTemp,
) error {
	if len(temps) == 0 {
		return nil
	}
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return err
	}
	defer root.close()
	for _, temp := range temps {
		if !isOwnedAtomicTempName(filepath.Base(temp.relativePath)) {
			return fmt.Errorf(
				"new Session rollback atomic temp %s is no longer a "+
					"regular Runtime-owned file",
				temp.relativePath,
			)
		}
		if err := root.removeRegular(
			temp.relativePath, &temp.identity,
		); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) removeNewSessionRoot(
	journal sessionMutationJournal,
) error {
	sessionID := journal.SessionID
	root := store.sessionDir(sessionID)
	store.hitMutationFailpoint("before_new_session_root_remove", "")
	sessions, err := store.openSessionsDirectory()
	if err != nil {
		return err
	}
	defer sessions.close()
	entry, err := sessions.statEntry(sessionID)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !entry.isDirectory() {
		return fmt.Errorf(
			"new Session root %s must be a directory, not a symlink", root,
		)
	}
	if entry.dev != journal.RootDevice || entry.ino != journal.RootInode {
		return fmt.Errorf(
			"new Session root %s does not match its mutation identity", root,
		)
	}
	rootIdentity := entry.identity()
	if err := sessions.removeEmptyDirectory(
		sessionID,
		&rootIdentity,
		func() error {
			store.hitMutationFailpoint(
				"before_new_session_root_unpublish", "",
			)
			return store.hitMutationErrorpoint(
				"before_new_session_root_unpublish", "",
			)
		},
	); err != nil {
		if errors.Is(err, os.ErrExist) ||
			errors.Is(err, syscall.ENOTEMPTY) {
			return fmt.Errorf(
				"new Session root %s contains unregistered content: %w",
				root, err,
			)
		}
		return err
	}
	store.forgetDirectoryIdentity(root)
	return nil
}

func (store *Store) removeMutationJournal(sessionID string) error {
	path := store.mutationJournalPath(sessionID)
	journalDirectory, err := store.openPinnedDirectory(store.journalDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer journalDirectory.close()
	entry, err := journalDirectory.statEntry(sessionID + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !entry.isRegular() || entry.nlink != 1 {
		return fmt.Errorf(
			"Session mutation journal %s must be a regular file, not a symlink",
			path,
		)
	}
	identity := entry.identity()
	return journalDirectory.removeRegular(sessionID+".json", &identity)
}

func (store *Store) mutationRelativePath(
	sessionID, path, kind string,
) (string, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return "", err
	}
	root := store.sessionDir(sessionID)
	relativePath, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if relativePath == "." ||
		relativePath == ".." ||
		filepath.IsAbs(relativePath) ||
		strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) ||
		filepath.Clean(relativePath) != relativePath {
		return "", fmt.Errorf(
			"Session mutation target escapes Session root: %s", path,
		)
	}
	parts := strings.Split(relativePath, string(os.PathSeparator))
	valid := false
	switch {
	case len(parts) == 1 &&
		(parts[0] == "session.json" ||
			parts[0] == "messages.jsonl" ||
			parts[0] == "events.jsonl"):
		valid = true
	case len(parts) == 3 && parts[0] == "turns" &&
		(parts[2] == "turn.json" ||
			parts[2] == "context-manifest.json"):
		valid = identity.Validate(parts[1], "turn") == nil
	case len(parts) == 2 && parts[0] == "executions" &&
		filepath.Ext(parts[1]) == ".json":
		valid = identity.Validate(
			strings.TrimSuffix(parts[1], ".json"), "execution",
		) == nil
	case len(parts) == 2 && parts[0] == "context" &&
		parts[1] == "current.json":
		valid = true
	}
	if !valid {
		return "", fmt.Errorf(
			"unsupported Session mutation target %q", relativePath,
		)
	}
	if kind == mutationAppend &&
		relativePath != "messages.jsonl" &&
		relativePath != "events.jsonl" {
		return "", fmt.Errorf(
			"Session append target %q is unsupported", relativePath,
		)
	}
	if kind == mutationReplace &&
		(relativePath == "messages.jsonl" ||
			relativePath == "events.jsonl") {
		return "", fmt.Errorf(
			"Session replace target %q is unsupported", relativePath,
		)
	}
	if kind != mutationAppend && kind != mutationReplace {
		return "", fmt.Errorf("unsupported Session mutation kind %q", kind)
	}
	return relativePath, nil
}

func (store *Store) mutationJournalPath(sessionID string) string {
	return filepath.Join(store.journalDir, sessionID+".json")
}

func (store *Store) hitMutationFailpoint(stage, relativePath string) {
	if store.mutationFailpoint != nil {
		store.mutationFailpoint(stage, relativePath)
	}
}

func (store *Store) hitMutationErrorpoint(
	stage, relativePath string,
) error {
	if store.mutationErrorpoint == nil {
		return nil
	}
	return store.mutationErrorpoint(stage, relativePath)
}
