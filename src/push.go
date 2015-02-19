// Copyright 2013 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package drive

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	gopath "path"
	"path/filepath"
	"sort"
	"strings"

	spinner "github.com/odeke-em/cli-spinner"
	"github.com/odeke-em/drive/config"
)

// Pushes to remote if local path exists and in a gd context. If path is a
// directory, it recursively pushes to the remote if there are local changes.
// It doesn't check if there are local changes if isForce is set.
func (g *Commands) Push() (err error) {
	defer g.clearMountPoints()

	root := g.context.AbsPathOf("")
	var cl []*Change

	fmt.Println("Resolving...")
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)

	// To Ensure mount points are cleared in the event of external exceptios
	go func() {
		_ = <-c
		g.clearMountPoints()
		os.Exit(1)
	}()

	for _, relToRootPath := range g.opts.Sources {
		fsPath := g.context.AbsPathOf(relToRootPath)
		ccl, cErr := g.changeListResolve(relToRootPath, fsPath, true)
		if cErr != nil {
			return cErr
		}
		if len(ccl) > 0 {
			cl = append(cl, ccl...)
		}
	}

	mount := g.opts.Mount
	if mount != nil {
		for _, mt := range mount.Points {
			ccl, cerr := lonePush(g, root, mt.Name, mt.MountPath)
			if cerr == nil {
				cl = append(cl, ccl...)
			}
		}
	}

	nonConflicts, conflicts := sift(cl)
	resolved, unresolved := resolveConflicts(conflicts, true, g.deserializeIndex)
	if len(unresolved) >= 1 {
		if conflictsPersist(unresolved) {
			return
		}
		for _, ch := range unresolved {
			resolved = append(resolved, ch)
		}
	}

	for _, ch := range resolved {
		nonConflicts = append(nonConflicts, ch)
	}

	ok := printChangeList(nonConflicts, g.opts.NoPrompt, g.opts.NoClobber)
	if ok {
		pushSize := reduceToSize(cl, true)

		quotaStatus, qErr := g.QuotaStatus(pushSize)
		if qErr != nil {
			return qErr
		}
		unSafe := false
		switch quotaStatus {
		case AlmostExceeded:
			fmt.Println("\033[92mAlmost exceeding your drive quota\033[00m")
		case Exceeded:
			fmt.Println("\033[91mThis change will exceed your drive quota\033[00m")
			unSafe = true
		}
		if unSafe {
			fmt.Printf(" projected size: %d (%d)\n", pushSize, prettyBytes(pushSize))
			if !promptForChanges() {
				return
			}
		}
		return g.playPushChangeList(nonConflicts)
	}
	return
}

func (g *Commands) PushPiped() (err error) {
	// Cannot push asynchronously because the pull order must be maintained
	for _, relToRootPath := range g.opts.Sources {
		rem, resErr := g.rem.FindByPath(relToRootPath)
		if resErr != nil && resErr != ErrPathNotExists {
			return resErr
		}

		if hasExportLinks(rem) {
            fmt.Printf("'%s' is a GoogleDoc/Sheet document cannot be pushed to raw.\n", relToRootPath)
			continue
		}

		base := filepath.Base(relToRootPath)
		local := fauxLocalFile(base)
		if rem == nil {
			rem = local
		}

		parentPath := g.parentPather(relToRootPath)
		parent, pErr := g.rem.FindByPath(parentPath)
		if pErr != nil {
			spin := spinner.New(10)
			spin.Start()
			parent, pErr = g.rem.mkdirAll(parentPath)
			spin.Stop()
			if pErr != nil || parent == nil {
				fmt.Printf("%s: %v", relToRootPath, pErr)
				return
			}
		}

		args := upsertOpt{
			parentId:       parent.Id,
			fsAbsPath:      relToRootPath,
			src:            rem,
			dest:           rem,
			mask:           g.opts.TypeMask,
			ignoreChecksum: g.opts.IgnoreChecksum,
		}

		rem, rErr := g.rem.upsertByComparison(os.Stdin, &args)
		if rErr != nil {
			fmt.Printf("%s: %v\n", relToRootPath, rErr)
			return rErr
		}
		if rem == nil {
			continue
		}
		index := rem.ToIndex()
		wErr := g.context.SerializeIndex(index, g.context.AbsPathOf(""))

		// TODO: Should indexing errors be reported?
		if wErr != nil {
			fmt.Printf("serializeIndex %s: %v\n", rem.Name, wErr)
		}
	}
	return
}

func (g *Commands) deserializeIndex(identifier string) *config.Index {
	index, err := g.context.DeserializeIndex(g.context.AbsPathOf(""), identifier)
	if err != nil {
		return nil
	}
	return index
}

func (g *Commands) playPushChangeList(cl []*Change) (err error) {
	g.taskStart(len(cl))

	// TODO: Only provide precedence ordering if all the other options are allowed
	// Currently noop on sorting by precedence
	if false && !g.opts.NoClobber {
		sort.Sort(ByPrecedence(cl))
	}

	for _, c := range cl {
		switch c.Op() {
		case OpMod:
			g.remoteMod(c)
		case OpModConflict:
			g.remoteMod(c)
		case OpAdd:
			g.remoteAdd(c)
		case OpDelete:
			g.remoteDelete(c)
		}
	}
	g.taskFinish()
	return err
}

func lonePush(g *Commands, parent, absPath, path string) (cl []*Change, err error) {
	r, err := g.rem.FindByPath(absPath)
	if err != nil && err != ErrPathNotExists {
		return
	}

	var l *File
	localinfo, _ := os.Stat(path)
	if localinfo != nil {
		l = NewLocalFile(path, localinfo)
	}

	return g.resolveChangeListRecv(true, parent, absPath, r, l)
}

func (g *Commands) parentPather(absPath string) string {
	p := strings.Split(absPath, "/")
	p = append([]string{"/"}, p[:len(p)-1]...)
	return gopath.Join(p...)
}

func (g *Commands) remoteMod(change *Change) (err error) {
	defer g.taskDone()

	absPath := g.context.AbsPathOf(change.Path)
	var parent *File
	if change.Dest != nil {
		change.Src.Id = change.Dest.Id // TODO: bad hack
	}

	parentPath := g.parentPather(change.Path)
	parent, err = g.rem.FindByPath(parentPath)

	if err != nil {
		spin := spinner.New(10)
		spin.Start()
		parent, err = g.rem.mkdirAll(parentPath)
		spin.Stop()
		if err != nil || parent == nil {
			fmt.Printf("%s: %v", change.Path, err)
			return
		}
	}

	args := upsertOpt{
		parentId:       parent.Id,
		fsAbsPath:      absPath,
		src:            change.Src,
		dest:           change.Dest,
		mask:           g.opts.TypeMask,
		ignoreChecksum: g.opts.IgnoreChecksum,
	}

	rem, err := g.rem.UpsertByComparison(&args)
	if err != nil {
		fmt.Printf("%s: %v\n", change.Path, err)
		return
	}
	if rem == nil {
		return
	}
	index := rem.ToIndex()
	wErr := g.context.SerializeIndex(index, g.context.AbsPathOf(""))

	// TODO: Should indexing errors be reported?
	if wErr != nil {
		fmt.Printf("serializeIndex %s: %v\n", rem.Name, wErr)
	}
	return
}

func (g *Commands) remoteAdd(change *Change) (err error) {
	return g.remoteMod(change)
}

func (g *Commands) indexAbsPath(fileId string) string {
	return config.IndicesAbsPath(g.context.AbsPathOf(""), fileId)
}

func (g *Commands) remoteUntrash(change *Change) (err error) {
	defer g.taskDone()

	return g.rem.Untrash(change.Src.Id)
}

func (g *Commands) remoteDelete(change *Change) (err error) {
	defer g.taskDone()

	err = g.rem.Trash(change.Dest.Id)
	if err != nil {
		return
	}

	indexPath := g.indexAbsPath(change.Dest.Id)
	if rmErr := os.Remove(indexPath); rmErr != nil {
		fmt.Printf("%s \"%s\": remove indexfile %v\n", change.Path, change.Dest.Id, rmErr)
	}
	return
}

func list(context *config.Context, p string, hidden bool) (fileChan chan *File, err error) {
	absPath := context.AbsPathOf(p)
	var f []os.FileInfo
	f, err = ioutil.ReadDir(absPath)
	fileChan = make(chan *File)
	if err != nil {
		close(fileChan)
		return
	}

	go func() {
		for _, file := range f {
			if file.Name() == config.GDDirSuffix {
				continue
			}
			if !isHidden(file.Name(), hidden) {
				fileChan <- NewLocalFile(gopath.Join(absPath, file.Name()), file)
			}
		}
		close(fileChan)
	}()
	return
}