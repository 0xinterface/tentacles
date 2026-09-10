package runner

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
)

// Identity is a resolved local account. A nil Credential means the current
// unprivileged identity already matches; no credential transition is needed.
type Identity struct {
	UID, GID   int
	Home       string
	Credential *syscall.Credential
}

// ResolveIdentity fails closed when an account is unknown or this process
// cannot assume it. Empty User selects the current identity for development.
func ResolveIdentity(spec Spec) (Identity, error) {
	var account *user.User
	var err error
	switch {
	case spec.User == "":
		account, err = user.Current()
	default:
		if _, parseErr := strconv.ParseUint(spec.User, 10, 32); parseErr == nil {
			account, err = user.LookupId(spec.User)
		} else {
			account, err = user.Lookup(spec.User)
		}
	}
	if err != nil {
		return Identity{}, fmt.Errorf("resolve runner user %q: %w", spec.User, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return Identity{}, err
	}
	gidText := account.Gid
	if spec.Group != "" {
		if _, err := strconv.ParseUint(spec.Group, 10, 32); err == nil {
			gidText = spec.Group
		} else {
			group, err := user.LookupGroup(spec.Group)
			if err != nil {
				return Identity{}, fmt.Errorf("resolve runner group: %w", err)
			}
			gidText = group.Gid
		}
	}
	gid, err := strconv.Atoi(gidText)
	if err != nil {
		return Identity{}, err
	}
	identity := Identity{UID: uid, GID: gid, Home: account.HomeDir}
	if os.Geteuid() != 0 {
		if uid != os.Geteuid() || gid != os.Getegid() {
			return Identity{}, fmt.Errorf("cannot assume runner uid=%d gid=%d without root", uid, gid)
		}
		return identity, nil
	}
	groups, err := account.GroupIds()
	if err != nil {
		return Identity{}, fmt.Errorf("resolve runner supplementary groups: %w", err)
	}
	credential := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}
	for _, group := range groups {
		n, err := strconv.ParseUint(group, 10, 32)
		if err != nil {
			return Identity{}, err
		}
		credential.Groups = append(credential.Groups, uint32(n))
	}
	identity.Credential = credential
	return identity, nil
}

// PrepareSlot transfers only a fresh, supervisor-owned tree. The top directory
// stays private until every child is prepared. Descriptor operations refuse
// symlinks and hardlinks so ownership cannot escape into a template or host file.
// JITPath, if inside the tree, remains owned by the supervisor.
func PrepareSlot(ctx context.Context, spec Spec, identity Identity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	top, err := os.OpenFile(spec.SlotDir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open fresh slot: %w", err)
	}
	defer top.Close()
	info, err := top.Stat()
	if err != nil {
		return err
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("slot must be owned by supervisor before start")
	}
	if err := top.Chmod(0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(spec.SlotDir)
	if err != nil {
		return err
	}
	defer root.Close()
	jitPath := filepath.Clean(spec.JITPath)
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == "." || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if filepath.Join(spec.SlotDir, path) == jitPath {
			return nil
		}
		file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() && (!info.Mode().IsRegular() || stat.Nlink != 1) {
			return fmt.Errorf("slot entry %q is not a private regular file", path)
		}
		if int(stat.Uid) != identity.UID || int(stat.Gid) != identity.GID {
			return file.Chown(identity.UID, identity.GID)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("prepare runner slot: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if int(stat.Uid) != identity.UID || int(stat.Gid) != identity.GID {
		if err := top.Chown(identity.UID, identity.GID); err != nil {
			return err
		}
	}
	return top.Chmod(info.Mode().Perm())
}

// ReadJIT validates and reads a private supervisor-owned source without
// following a final symlink or blocking on a FIFO.
func ReadJIT(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink != 1 || int(stat.Uid) != os.Geteuid() {
		return nil, fmt.Errorf("JIT source must be a supervisor-owned private 0600 regular file")
	}
	if info.Size() > 1024*1024 {
		return nil, fmt.Errorf("JIT source exceeds 1 MiB")
	}
	data := make([]byte, info.Size())
	_, err = file.ReadAt(data, 0)
	if err != nil {
		return nil, err
	}
	return data, nil
}
