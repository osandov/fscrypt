/*
 * mountpoint.go - Contains all the functionality for finding mountpoints and
 * using UUIDs to refer to them. Specifically, we can find the mountpoint of a
 * path, get info about a mountpoint, and find mountpoints with a specific UUID.
 *
 * Copyright 2017 Google Inc.
 * Author: Joe Richey (joerichey@google.com)
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not
 * use this file except in compliance with the License. You may obtain a copy of
 * the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
 * License for the specific language governing permissions and limitations under
 * the License.
 */

package filesystem

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/errors"
)

var (
	// These maps hold data about the state of the system's mountpoints.
	mountsByPath   map[string]*Mount
	mountsByDevice map[string][]*Mount
	// Used to make the mount functions thread safe
	mountMutex sync.Mutex
	// True if the maps have been successfully initialized.
	mountsInitialized bool
	// Supported tokens for filesystem links
	uuidToken = "UUID"
	// Location to perform UUID lookup
	uuidDirectory = "/dev/disk/by-uuid"
)

// Unescape octal-encoded escape sequences in a string from the mountinfo file.
// The kernel encodes the ' ', '\t', '\n', and '\\' bytes this way.  This
// function exactly inverts what the kernel does, including by preserving
// invalid UTF-8.
func unescapeString(str string) string {
	var sb strings.Builder
	for i := 0; i < len(str); i++ {
		b := str[i]
		if b == '\\' && i+3 < len(str) {
			if parsed, err := strconv.ParseInt(str[i+1:i+4], 8, 8); err == nil {
				b = uint8(parsed)
				i += 3
			}
		}
		sb.WriteByte(b)
	}
	return sb.String()
}

// Parse one line of /proc/self/mountinfo.
//
// The line contains the following space-separated fields:
//	[0] mount ID
//	[1] parent ID
//	[2] major:minor
//	[3] root
//	[4] mount point
//	[5] mount options
//	[6...n-1] optional field(s)
//	[n] separator
//	[n+1] filesystem type
//	[n+2] mount source
//	[n+3] super options
//
// For more details, see https://www.kernel.org/doc/Documentation/filesystems/proc.txt
func parseMountInfoLine(line string) *Mount {
	fields := strings.Split(line, " ")
	if len(fields) < 10 {
		return nil
	}

	// Count the optional fields.  In case new fields are appended later,
	// don't simply assume that n == len(fields) - 4.
	n := 6
	for fields[n] != "-" {
		n++
		if n >= len(fields) {
			return nil
		}
	}
	if n+3 >= len(fields) {
		return nil
	}

	var mnt *Mount = &Mount{}
	mnt.Path = unescapeString(fields[4])
	mnt.FilesystemType = unescapeString(fields[n+1])
	mnt.Device = unescapeString(fields[n+2])
	return mnt
}

// loadMountInfo populates the Mount mappings by parsing /proc/self/mountinfo.
// It returns an error if the Mount mappings cannot be populated.
func loadMountInfo() error {
	if mountsInitialized {
		return nil
	}
	mountsByPath = make(map[string]*Mount)
	mountsByDevice = make(map[string][]*Mount)

	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		mnt := parseMountInfoLine(line)
		if mnt == nil {
			log.Printf("ignoring invalid mountinfo line %q", line)
			continue
		}

		// Skip invalid mountpoints
		var err error
		if mnt.Path, err = canonicalizePath(mnt.Path); err != nil {
			log.Printf("getting mnt_dir: %v", err)
			continue
		}
		// We can only use mountpoints that are directories for fscrypt.
		if !isDir(mnt.Path) {
			log.Printf("ignoring mountpoint %q because it is not a directory", mnt.Path)
			continue
		}

		// Note this overrides the info if we have seen the mountpoint
		// earlier in the file. This is correct behavior because the
		// filesystems are listed in mount order.
		mountsByPath[mnt.Path] = mnt

		mnt.Device, err = canonicalizePath(mnt.Device)
		// Only use real valid devices (unlike cgroups, tmpfs, ...)
		if err == nil && isDevice(mnt.Device) {
			mountsByDevice[mnt.Device] = append(mountsByDevice[mnt.Device], mnt)
		} else {
			mnt.Device = ""
		}
	}
	mountsInitialized = true
	return nil
}

// AllFilesystems lists all the Mounts on the current system ordered by path.
// Use CheckSetup() to see if they are used with fscrypt.
func AllFilesystems() ([]*Mount, error) {
	mountMutex.Lock()
	defer mountMutex.Unlock()
	if err := loadMountInfo(); err != nil {
		return nil, err
	}

	mounts := make([]*Mount, 0, len(mountsByPath))
	for _, mount := range mountsByPath {
		mounts = append(mounts, mount)
	}

	sort.Sort(PathSorter(mounts))
	return mounts, nil
}

// UpdateMountInfo updates the filesystem mountpoint maps with the current state
// of the filesystem mountpoints. Returns error if the initialization fails.
func UpdateMountInfo() error {
	mountMutex.Lock()
	defer mountMutex.Unlock()
	mountsInitialized = false
	return loadMountInfo()
}

// FindMount returns the corresponding Mount object for some path in a
// filesystem. Note that in the case of a bind mounts there may be two Mount
// objects for the same underlying filesystem. An error is returned if the path
// is invalid or we cannot load the required mount data. If a filesystem has
// been updated since the last call to one of the mount functions, run
// UpdateMountInfo to see changes.
func FindMount(path string) (*Mount, error) {
	path, err := canonicalizePath(path)
	if err != nil {
		return nil, err
	}

	mountMutex.Lock()
	defer mountMutex.Unlock()
	if err = loadMountInfo(); err != nil {
		return nil, err
	}

	// Traverse up the directory tree until we find a mountpoint
	for {
		if mnt, ok := mountsByPath[path]; ok {
			return mnt, nil
		}

		// Move to the parent directory unless we have reached the root.
		parent := filepath.Dir(path)
		if parent == path {
			return nil, errors.Wrap(ErrNotAMountpoint, path)
		}
		path = parent
	}
}

// GetMount returns the Mount object with a matching mountpoint. An error is
// returned if the path is invalid or we cannot load the required mount data. If
// a filesystem has been updated since the last call to one of the mount
// functions, run UpdateMountInfo to see changes.
func GetMount(mountpoint string) (*Mount, error) {
	mountpoint, err := canonicalizePath(mountpoint)
	if err != nil {
		return nil, err
	}

	mountMutex.Lock()
	defer mountMutex.Unlock()
	if err = loadMountInfo(); err != nil {
		return nil, err
	}

	if mnt, ok := mountsByPath[mountpoint]; ok {
		return mnt, nil
	}

	return nil, errors.Wrap(ErrNotAMountpoint, mountpoint)
}

// getMountsFromLink returns the Mount objects which match the provided link.
// This link is formatted as a tag (e.g. <token>=<value>) similar to how they
// appear in "/etc/fstab". Currently, only "UUID" tokens are supported. Note
// that this can match multiple Mounts (due to the existence of bind mounts). An
// error is returned if the link is invalid or we cannot load the required mount
// data. If a filesystem has been updated since the last call to one of the
// mount functions, run UpdateMountInfo to see the change.
func getMountsFromLink(link string) ([]*Mount, error) {
	// Parse the link
	linkComponents := strings.Split(link, "=")
	if len(linkComponents) != 2 {
		return nil, errors.Wrapf(ErrFollowLink, "link %q format is invalid", link)
	}
	token := linkComponents[0]
	value := linkComponents[1]
	if token != uuidToken {
		return nil, errors.Wrapf(ErrFollowLink, "token type %q not supported", token)
	}

	// See if UUID points to an existing device
	searchPath := filepath.Join(uuidDirectory, value)
	if filepath.Base(searchPath) != value {
		return nil, errors.Wrapf(ErrFollowLink, "value %q is not a UUID", value)
	}
	devicePath, err := canonicalizePath(searchPath)
	if err != nil {
		return nil, errors.Wrapf(ErrFollowLink, "no device with UUID %q", value)
	}

	// Lookup mountpoints for device in global store
	mountMutex.Lock()
	defer mountMutex.Unlock()
	if err := loadMountInfo(); err != nil {
		return nil, err
	}
	mnts, ok := mountsByDevice[devicePath]
	if !ok {
		return nil, errors.Wrapf(ErrFollowLink, "no mounts for device %q", devicePath)
	}
	return mnts, nil
}

// makeLink returns a link of the form <token>=<value> where value is the tag
// value for the Mount's device. Currently, only "UUID" tokens are supported. An
// error is returned if the mount has no device, or no UUID.
func makeLink(mnt *Mount, token string) (string, error) {
	if token != uuidToken {
		return "", errors.Wrapf(ErrMakeLink, "token type %q not supported", token)
	}
	if mnt.Device == "" {
		return "", errors.Wrapf(ErrMakeLink, "no device for mount %q", mnt.Path)
	}

	dirContents, err := ioutil.ReadDir(uuidDirectory)
	if err != nil {
		return "", errors.Wrap(ErrMakeLink, err.Error())
	}
	for _, fileInfo := range dirContents {
		if fileInfo.Mode()&os.ModeSymlink == 0 {
			continue // Only interested in UUID symlinks
		}
		uuid := fileInfo.Name()
		devicePath, err := canonicalizePath(filepath.Join(uuidDirectory, uuid))
		if err != nil {
			log.Print(err)
			continue
		}
		if mnt.Device == devicePath {
			return fmt.Sprintf("%s=%s", uuidToken, uuid), nil
		}
	}
	return "", errors.Wrapf(ErrMakeLink, "device %q has no UUID", mnt.Device)
}
