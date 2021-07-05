// Copyright 2017 HootSuite Media Inc.
//
// Licensed under the Apache License, Version 2.0 (the License);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an AS IS BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// Modified hereafter by contributors to runatlantis/atlantis.

package events

import (
	"fmt"
	"strings"
	"sync"
)

//go:generate pegomock generate --use-experimental-model-gen --package mocks -o mocks/mock_working_dir_locker.go WorkingDirLocker

// WorkingDirLocker is used to prevent multiple commands from executing
// at the same time for a single repo, pull, and workspace. We need to prevent
// this from happening because a specific repo/pull/workspace has a single workspace
// on disk and we haven't written Atlantis (yet) to handle concurrent execution
// within this workspace.
type WorkingDirLocker interface {
	// TryLock tries to acquire a lock for this repo, workspace and pull.
	// It returns a function that should be used to unlock the workspace and
	// an error if the workspace is already locked. The error is expected to
	// be printed to the pull request.
	TryLock(repoFullName string, pullNum int, workspace string) (func(), error)
	// TryLockPull tries to acquire a lock for all the workspaces in this repo
	// and pull.
	// It returns a function that should be used to unlock the workspace and
	// an error if the workspace is already locked. The error is expected to
	// be printed to the pull request.
	TryLockPull(repoFullName string, pullNum int) (func(), error)
	// 
	TryLockWithCommit(repoFullName string, pullNum int, pullHeadCommit string, workspace string) (func(), func(), error)
}

// DefaultWorkingDirLocker implements WorkingDirLocker.
type DefaultWorkingDirLocker struct {
	// mutex prevents against multiple threads calling functions on this struct
	// concurrently. It's only used for entry/exit to each function.
	mutex sync.Mutex
	// locks is a list of the keys that are locked. We then use prefix
	// matching to determine if something is locked. It's naive but that's okay
	// because there won't be many locks at one time.
	locks []string

	commitWaitGroupMap map[string]sync.WaitGroup
}

// NewDefaultWorkingDirLocker is a constructor.
func NewDefaultWorkingDirLocker() *DefaultWorkingDirLocker {
	return &DefaultWorkingDirLocker{}
}

func (d *DefaultWorkingDirLocker) TryLockPull(repoFullName string, pullNum int) (func(), error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	pullKey := d.pullKey(repoFullName, pullNum)
	for _, l := range d.locks {
		if l == pullKey || strings.HasPrefix(l, pullKey+"/") {
			return func() {}, fmt.Errorf("The Atlantis working dir is currently locked by another" +
				" command that is running for this pull request.\n" +
				"Wait until the previous command is complete and try again.")
		}
	}
	d.locks = append(d.locks, pullKey)
	return func() {
		d.unlockPull(repoFullName, pullNum)
	}, nil
}

func (d *DefaultWorkingDirLocker) TryLock(repoFullName string, pullNum int, workspace string) (func(), error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	pullKey := d.pullKey(repoFullName, pullNum)
	workspaceKey := d.workspaceKey(repoFullName, pullNum, workspace)
	for _, l := range d.locks {
		if l == pullKey || l == workspaceKey {
			return func() {}, fmt.Errorf("The %s workspace is currently locked by another"+
				" command that is running for this pull request.\n"+
				"Wait until the previous command is complete and try again.", workspace)
		}
	}
	d.locks = append(d.locks, workspaceKey)
	return func() {
		d.unlock(repoFullName, pullNum, workspace)
	}, nil
}

func (d *DefaultWorkingDirLocker) TryLockWithCommit(repoFullName string, pullNum int, pullHeadCommit string, workspace string) (func(), func(), error) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	pullKey := d.pullKey(repoFullName, pullNum)
	workspaceKey := d.workspaceKey(repoFullName, pullNum, workspace)
	commitKey := d.commitKey(repoFullName, pullNum, pullHeadCommit, workspace)
	alreadyLocked := false
	for _, l := range d.locks {
		alreadyLocked = l == commitKey
		if alreadyLocked {
			break
		}
		if l == pullKey || strings.HasPrefix(l, workspaceKey + "@") {
			return func() {}, nil, fmt.Errorf("The %s workspace is currently locked by another"+
				" command that is running for this pull request.\n"+
				"Wait until the previous command is complete and try again.", workspace)
		}
	}
	d.locks = append(d.locks, commitKey)
	if alreadyLocked {
		wg := d.commitWaitGroupMap[commitKey]
		return func() {
			d.unlock(repoFullName, pullNum, workspace)
		}, func() {
			wg.Wait()
		}, nil
	} else {
		var wg sync.WaitGroup
		wg.Add(1)
		if (d.commitWaitGroupMap == nil) {
			d.commitWaitGroupMap = make(map[string]sync.WaitGroup)
		}
		d.commitWaitGroupMap[commitKey] = wg
		return func() {
			d.unlock(repoFullName, pullNum, workspace)
			wg.Done()
			d.mutex.Lock()
			defer d.mutex.Unlock()
			delete(d.commitWaitGroupMap, commitKey)
		}, nil, nil
	}
}

// Unlock unlocks the workspace for this pull.
func (d *DefaultWorkingDirLocker) unlock(repoFullName string, pullNum int, workspace string) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	workspaceKey := d.workspaceKey(repoFullName, pullNum, workspace)
	d.removeLock(workspaceKey)
}

// Unlock unlocks all workspaces for this pull.
func (d *DefaultWorkingDirLocker) unlockPull(repoFullName string, pullNum int) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	pullKey := d.pullKey(repoFullName, pullNum)
	d.removeLock(pullKey)
}

// Unlock unlocks all workspaces for this pull.
func (d *DefaultWorkingDirLocker) unlockCommit(repoFullName string, pullNum int, pullHeadCommit string, workspace string) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	commitKey := d.commitKey(repoFullName, pullNum, pullHeadCommit, workspace)
	d.removeLock(commitKey)
}

func (d *DefaultWorkingDirLocker) removeLock(key string) {
	var newLocks []string
	for _, l := range d.locks {
		if l != key {
			newLocks = append(newLocks, l)
			break
		}
	}
	d.locks = newLocks
}

func (d *DefaultWorkingDirLocker) workspaceKey(repo string, pull int, workspace string) string {
	return fmt.Sprintf("%s/%s", d.pullKey(repo, pull), workspace)
}

func (d *DefaultWorkingDirLocker) pullKey(repo string, pull int) string {
	return fmt.Sprintf("%s/%d", repo, pull)
}

func (d *DefaultWorkingDirLocker) commitKey(repo string, pull int, commit string, workspace string) string {
	return fmt.Sprintf("%s/%s@%s", d.pullKey(repo, pull), workspace, commit)
}
