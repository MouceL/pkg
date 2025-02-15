// Copyright 2018 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package filewatcher

import (
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	. "github.com/onsi/gomega"
)

var rootTmpDir string

func init() {
	var err error
	if rootTmpDir, err = ioutil.TempDir("", "filewatcher_test"); err != nil {
		panic(err)
	}
}

func newWatchFileImpl() (string, func(), error) {
	watchDir, err := ioutil.TempDir(rootTmpDir, "")
	if err != nil {
		return "", nil, err
	}

	watchFile := path.Join(watchDir, "test.conf")
	err = ioutil.WriteFile(watchFile, []byte("foo: bar\n"), 0o640)
	if err != nil {
		_ = os.RemoveAll(watchDir)
		return "", nil, err
	}
	cleanup := func() {
		_ = os.RemoveAll(watchDir)
	}

	return watchFile, cleanup, nil
}

func newWatchFile(t *testing.T) (string, func()) {
	g := NewGomegaWithT(t)
	name, cleanup, err := newWatchFileImpl()
	g.Expect(err).NotTo(HaveOccurred())
	return name, cleanup
}

func newWatchFileThatDoesNotExist(t *testing.T) (string, func()) {
	g := NewGomegaWithT(t)

	watchDir, err := ioutil.TempDir("", "")
	g.Expect(err).NotTo(HaveOccurred())

	watchFile := path.Join(watchDir, "test.conf")

	cleanup := func() {
		os.RemoveAll(watchDir)
	}

	return watchFile, cleanup
}

// newTwoWatchFile returns with two watch files that exist in the same base dir.
func newTwoWatchFile(t *testing.T) (string, string, func()) {
	g := NewGomegaWithT(t)

	watchDir, err := ioutil.TempDir("", "")
	g.Expect(err).NotTo(HaveOccurred())

	watchFile1 := path.Join(watchDir, "test1.conf")
	err = ioutil.WriteFile(watchFile1, []byte("foo: bar\n"), 0o640)
	g.Expect(err).NotTo(HaveOccurred())

	watchFile2 := path.Join(watchDir, "test2.conf")
	err = ioutil.WriteFile(watchFile2, []byte("foo: baz\n"), 0o640)
	g.Expect(err).NotTo(HaveOccurred())

	cleanup := func() {
		os.RemoveAll(watchDir)
	}

	return watchFile1, watchFile2, cleanup
}

// newSymlinkedWatchFile simulates the behavior of k8s configmap/secret.
// Path structure looks like:
//      <watchDir>/test.conf
//                   ^
//                   |
// <watchDir>/data/test.conf
//             ^
//             |
// <watchDir>/data1/test.conf
func newSymlinkedWatchFile(t *testing.T) (string, string, func()) {
	g := NewGomegaWithT(t)

	watchDir, err := ioutil.TempDir("", "")
	g.Expect(err).NotTo(HaveOccurred())

	dataDir1 := path.Join(watchDir, "data1")
	err = os.Mkdir(dataDir1, 0o777)
	g.Expect(err).NotTo(HaveOccurred())

	realTestFile := path.Join(dataDir1, "test.conf")
	t.Logf("Real test file location: %s\n", realTestFile)
	err = ioutil.WriteFile(realTestFile, []byte("foo: bar\n"), 0o640)
	g.Expect(err).NotTo(HaveOccurred())

	cleanup := func() {
		os.RemoveAll(watchDir)
	}
	// Now, symlink the tmp `data1` dir to `data` in the baseDir
	os.Symlink(dataDir1, path.Join(watchDir, "data"))
	// And link the `<watchdir>/datadir/test.conf` to `<watchdir>/test.conf`
	watchFile := path.Join(watchDir, "test.conf")
	os.Symlink(path.Join(watchDir, "data", "test.conf"), watchFile)
	fmt.Printf("Watch file location: %s\n", path.Join(watchDir, "test.conf"))
	return watchDir, watchFile, cleanup
}

func TestWatchFile(t *testing.T) {
	t.Run("file content changed", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// Given a file being watched
		watchFile, cleanup := newWatchFile(t)
		defer cleanup()
		_, err := os.Stat(watchFile)
		g.Expect(err).NotTo(HaveOccurred())

		w := NewWatcher()
		w.Add(watchFile)
		events := w.Events(watchFile)

		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			<-events
			wg.Done()
		}()

		// Overwriting the file and waiting its event to be received.
		err = ioutil.WriteFile(watchFile, []byte("foo: baz\n"), 0o640)
		g.Expect(err).NotTo(HaveOccurred())
		wg.Wait()

		_ = w.Close()
	})

	t.Run("link to real file changed (for k8s configmap/secret path)", func(t *testing.T) {
		// skip if not executed on Linux
		if runtime.GOOS != "linux" {
			t.Skipf("Skipping test as symlink replacements don't work on non-linux environment...")
		}
		g := NewGomegaWithT(t)

		watchDir, watchFile, cleanup := newSymlinkedWatchFile(t)
		defer cleanup()

		w := NewWatcher()
		w.Add(watchFile)
		events := w.Events(watchFile)

		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			<-events
			wg.Done()
		}()

		// Link to another `test.conf` file
		dataDir2 := path.Join(watchDir, "data2")
		err := os.Mkdir(dataDir2, 0o777)
		g.Expect(err).NotTo(HaveOccurred())

		watchFile2 := path.Join(dataDir2, "test.conf")
		err = ioutil.WriteFile(watchFile2, []byte("foo: baz\n"), 0o640)
		g.Expect(err).NotTo(HaveOccurred())

		// change the symlink using the `ln -sfn` command
		err = exec.Command("ln", "-sfn", dataDir2, path.Join(watchDir, "data")).Run()
		g.Expect(err).NotTo(HaveOccurred())

		// Wait its event to be received.
		wg.Wait()

		_ = w.Close()
	})

	t.Run("file added later", func(t *testing.T) {
		g := NewGomegaWithT(t)

		// Given a file being watched
		watchFile, cleanup := newWatchFileThatDoesNotExist(t)
		defer cleanup()

		w := NewWatcher()
		w.Add(watchFile)
		events := w.Events(watchFile)

		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			<-events
			wg.Done()
		}()

		// Overwriting the file and waiting its event to be received.
		err := ioutil.WriteFile(watchFile, []byte("foo: baz\n"), 0o640)
		g.Expect(err).NotTo(HaveOccurred())
		wg.Wait()

		_ = w.Close()
	})
}

func TestWatcherLifecycle(t *testing.T) {
	g := NewGomegaWithT(t)

	watchFile1, watchFile2, cleanup := newTwoWatchFile(t)
	defer cleanup()

	w := NewWatcher()

	// Validate Add behavior
	err := w.Add(watchFile1)
	g.Expect(err).NotTo(HaveOccurred())
	err = w.Add(watchFile2)
	g.Expect(err).NotTo(HaveOccurred())

	// Validate events and errors channel are fulfilled.
	events1 := w.Events(watchFile1)
	g.Expect(events1).NotTo(BeNil())
	events2 := w.Events(watchFile2)
	g.Expect(events2).NotTo(BeNil())

	errors1 := w.Errors(watchFile1)
	g.Expect(errors1).NotTo(BeNil())
	errors2 := w.Errors(watchFile2)
	g.Expect(errors2).NotTo(BeNil())

	// Validate Remove behavior
	err = w.Remove(watchFile1)
	g.Expect(err).NotTo(HaveOccurred())
	events1 = w.Events(watchFile1)
	g.Expect(events1).To(BeNil())
	errors1 = w.Errors(watchFile1)
	g.Expect(errors1).To(BeNil())
	events2 = w.Events(watchFile2)
	g.Expect(events2).NotTo(BeNil())
	errors2 = w.Errors(watchFile2)
	g.Expect(errors2).NotTo(BeNil())

	fmt.Printf("2\n")
	// Validate Close behavior
	err = w.Close()
	g.Expect(err).NotTo(HaveOccurred())
	events1 = w.Events(watchFile1)
	g.Expect(events1).To(BeNil())
	errors1 = w.Errors(watchFile1)
	g.Expect(errors1).To(BeNil())
	events2 = w.Events(watchFile2)
	g.Expect(events2).To(BeNil())
	errors2 = w.Errors(watchFile2)
	g.Expect(errors2).To(BeNil())
}

func TestErrors(t *testing.T) {
	w := NewWatcher()

	if ch := w.Errors("XYZ"); ch != nil {
		t.Error("Expected no channel")
	}

	if ch := w.Events("XYZ"); ch != nil {
		t.Error("Expected no channel")
	}

	name, _ := newWatchFile(t)
	_ = w.Add(name)
	_ = w.Remove(name)

	if ch := w.Errors("XYZ"); ch != nil {
		t.Error("Expected no channel")
	}

	if ch := w.Events(name); ch != nil {
		t.Error("Expected no channel")
	}

	_ = w.Close()

	if err := w.Add(name); err == nil {
		t.Error("Expecting error")
	}

	if err := w.Remove(name); err == nil {
		t.Error("Expecting error")
	}

	if ch := w.Errors(name); ch != nil {
		t.Error("Expecting nil")
	}

	if ch := w.Events(name); ch != nil {
		t.Error("Expecting nil")
	}
}

func TestBadWatcher(t *testing.T) {
	w := NewWatcher()
	w.(*fileWatcher).funcs.newWatcher = func() (*fsnotify.Watcher, error) {
		return nil, errors.New("FOOBAR")
	}

	name, _ := newWatchFile(t)
	if err := w.Add(name); err == nil {
		t.Errorf("Expecting error, got nil")
	}
	if err := w.Close(); err != nil {
		t.Errorf("Expecting nil, got %v", err)
	}
}

func TestBadAddWatcher(t *testing.T) {
	w := NewWatcher()
	w.(*fileWatcher).funcs.addWatcherPath = func(*fsnotify.Watcher, string) error {
		return errors.New("FOOBAR")
	}

	name, _ := newWatchFile(t)
	if err := w.Add(name); err == nil {
		t.Errorf("Expecting error, got nil")
	}
	if err := w.Close(); err != nil {
		t.Errorf("Expecting nil, got %v", err)
	}
}

func TestDuplicateAdd(t *testing.T) {
	w := NewWatcher()

	name, _ := newWatchFile(t)

	if err := w.Add(name); err != nil {
		t.Errorf("Expecting nil, got %v", err)
	}

	if err := w.Add(name); err == nil {
		t.Errorf("Expecting error, got nil")
	}

	_ = w.Close()
}

func TestBogusRemove(t *testing.T) {
	w := NewWatcher()

	name, _ := newWatchFile(t)
	if err := w.Remove(name); err == nil {
		t.Errorf("Expecting error, got nil")
	}

	_ = w.Close()
}

type churnFile struct {
	file        string
	cleanupFile func()
	eventDoneCh chan struct{}
}

func (c *churnFile) active() bool {
	return c.cleanupFile != nil
}

func (c *churnFile) create(w FileWatcher) error {
	if c.active() {
		panic("File is currently active")
	}
	f, cl, err := newWatchFileImpl()
	if err != nil {
		return err
	}

	if err = w.Add(f); err != nil {
		cl()
		return err
	}

	events := w.Events(f)
	errors := w.Errors(f)
	eventDoneCh := make(chan struct{})

	c.eventDoneCh = eventDoneCh
	c.file = f
	c.cleanupFile = cl

	go func() {
		// sporadically read events
		for {
			<-time.After(time.Millisecond * time.Duration(rand.Int31n(5)))
			select {
			case <-eventDoneCh:
				return
			case <-events: // read and discard events
			case <-errors:
			}
		}
	}()

	return nil
}

func changeFileContents(file string) {
	l := rand.Int31n(4 * 1024)
	b := make([]byte, l)
	for i := 0; i < int(l); i++ {
		b[i] = byte(rand.Int31n(255))
	}

	_ = ioutil.WriteFile(file, b, 0o777)
}

func (c *churnFile) modify() {
	if !c.active() {
		panic("file is not active")
	}
	changeFileContents(c.file)
}

func (c *churnFile) remove(w FileWatcher) error {
	if !c.active() {
		panic("file is not active")
	}
	close(c.eventDoneCh)
	err := w.Remove(c.file)
	c.cleanupFile()

	c.file = ""
	c.cleanupFile = nil
	c.eventDoneCh = nil

	return err
}

func TestChurn(t *testing.T) {
	g := NewGomegaWithT(t)

	duration := time.Second * 5
	workers := 5
	filesPerWorker := 15

	w := NewWatcher()
	defer func() { _ = w.Close() }()

	done := make(chan struct{})
	go func() {
		<-time.After(duration)
		close(done)
	}()

	// create a bunch of workers and perform add/modify operations
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()
			files := make([]*churnFile, filesPerWorker)
			for j := 0; j < filesPerWorker; j++ {
				files[j] = &churnFile{}
			}

			defer func() {
				for _, e := range files {
					if e.active() {
						_ = e.remove(w)
					}
				}
			}()

			for {
				select {
				case <-done:
					return
				default:
				}

				f := files[rand.Int31n(int32(filesPerWorker))]
				switch rand.Int31n(2) {
				case 0: // modify or create
					if f.active() {
						f.modify()
					} else {
						err := f.create(w)
						g.Expect(err).To(BeNil())
					}

				case 1: // create or delete
					if f.active() {
						err := f.remove(w)
						g.Expect(err).To(BeNil())
					} else {
						err := f.create(w)
						g.Expect(err).To(BeNil())
					}
				}
			}
		}()
	}

	wg.Wait()
}
