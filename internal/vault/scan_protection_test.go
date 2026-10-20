package vault

import (
	"context"
	"errors"
	"testing"
	"time"

	"pikpakvault/internal/pikpak"
)

type scanHookDrive struct {
	*fakeDrive
	onList func(string, pikpak.Page) (pikpak.Page, error)
}

func (f *scanHookDrive) List(ctx context.Context, parent, page string) (pikpak.Page, error) {
	result, err := f.fakeDrive.List(ctx, parent, page)
	if err == nil && f.onList != nil {
		return f.onList(parent, result)
	}
	return result, err
}

func scanProtectionFixture(t *testing.T) (*App, *fakeDrive, Node, Node, Node) {
	t.Helper()
	a, f := testApp(t)
	root, err := a.ensureRoot(context.Background(), f, "a")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := f.Mkdir(context.Background(), root, "目标目录")
	if err != nil {
		t.Fatal(err)
	}
	nodes := []Node{
		{ID: "parent", ParentID: "root", Name: parent.Name, Kind: "folder"},
		{ID: "upload", ParentID: "parent", Name: "video.mp4", Kind: "file", Size: 10, Hash: "hash"},
		{ID: "other", ParentID: "root", Name: "other.mp4", Kind: "file", Size: 10, Hash: "other"},
	}
	files := []pikpak.File{
		parent,
		{ID: "remote-upload", ParentID: parent.ID, Name: nodes[1].Name, Kind: "drive#file", Size: 10, Hash: "hash", Phase: "PHASE_TYPE_COMPLETE"},
		{ID: "remote-other", ParentID: root, Name: nodes[2].Name, Kind: "drive#file", Size: 10, Hash: "other", Phase: "PHASE_TYPE_COMPLETE"},
	}
	for i, n := range nodes {
		if err = a.Store.InsertNode(n); err != nil {
			t.Fatal(err)
		}
		file := files[i]
		f.files[file.ID] = file
		if err = a.Store.Bind("a", n.ID, file.ID, "present", file.Name, file.ParentID, ""); err != nil {
			t.Fatal(err)
		}
		nodes[i], err = a.Store.Node(n.ID, "a")
		if err != nil {
			t.Fatal(err)
		}
	}
	return a, f, nodes[0], nodes[1], nodes[2]
}

func assertScanState(t *testing.T, a *App, id, want string) {
	t.Helper()
	n, err := a.Store.Node(id, "a")
	if err != nil || n.State != want {
		t.Fatalf("node %s state=%s want=%s err=%v", id, n.State, want, err)
	}
}

func TestConcurrentScanPreservesTransferAndImplicitParents(t *testing.T) {
	a, f, parent, file, other := scanProtectionFixture(t)
	upload, err := a.Store.NewJob("a", "teldrive_upload", "upload", JobData{NodeIDs: []string{file.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// The upload explicitly owns only its file; rebuilding its parent is
	// implicit. The source directory may no longer be the current destination.
	for _, n := range []Node{parent, file} {
		remote := f.files[n.RemoteID]
		remote.Trashed = true
		f.files[n.RemoteID] = remote
		if err = a.Store.State("a", n.ID, "pending"); err != nil {
			t.Fatal(err)
		}
	}
	delete(f.files, other.RemoteID)
	c := &scanHookDrive{fakeDrive: f}
	c.onList = func(_ string, page pikpak.Page) (pikpak.Page, error) {
		// Even if ownership ends before the commit, the starting snapshot must
		// still prevent this scan from publishing its earlier observations.
		upload.State = "completed"
		return page, a.Store.SaveJob(&upload, nil)
	}
	if err = a.checkLibrary(context.Background(), c, &Job{AccountID: "a"}); err != nil {
		t.Fatal(err)
	}
	assertScanState(t, a, file.ID, "pending")
	assertScanState(t, a, parent.ID, "pending")
	assertScanState(t, a, other.ID, "missing")

	// Completion does not permanently exempt the file or its parents.
	if err = a.checkLibrary(context.Background(), f, &Job{AccountID: "a"}); err != nil {
		t.Fatal(err)
	}
	assertScanState(t, a, file.ID, "missing")
	assertScanState(t, a, parent.ID, "missing")
}

func TestConcurrentScanProtectsNewlyReservedNodesAtCommit(t *testing.T) {
	a, f, parent, file, other := scanProtectionFixture(t)
	delete(f.files, file.RemoteID)
	delete(f.files, other.RemoteID)
	c := &scanHookDrive{fakeDrive: f}
	created := false
	c.onList = func(_ string, page pikpak.Page) (pikpak.Page, error) {
		if !created {
			created = true
			_, err := a.Store.NewJob("a", "recover", "repair", JobData{NodeIDs: []string{file.ID}, ExactNodes: true})
			return page, err
		}
		return page, nil
	}
	if err := a.checkLibrary(context.Background(), c, &Job{AccountID: "a"}); err != nil {
		t.Fatal(err)
	}
	assertScanState(t, a, file.ID, "present")
	assertScanState(t, a, parent.ID, "present")
	assertScanState(t, a, other.ID, "missing")
}

func TestConcurrentScanProtectionFollowsLatestParentAndAccount(t *testing.T) {
	a, _, parent, file, _ := scanProtectionFixture(t)
	if err := a.Store.InsertNode(Node{ID: "moved-parent", ParentID: "root", Name: "新位置", Kind: "folder"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.NewJob("a", "teldrive_upload", "upload", JobData{NodeIDs: []string{file.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.DB.Exec(`UPDATE nodes SET parent_id='moved-parent',revision=revision+1 WHERE id=?`, file.ID); err != nil {
		t.Fatal(err)
	}
	protected, err := scanProtectedNodes(a.Store.DB, "a")
	if err != nil || !protected[file.ID] || !protected["moved-parent"] || protected[parent.ID] {
		t.Fatalf("protection did not follow current ancestry: %v, %v", protected, err)
	}
	protected, err = scanProtectedNodes(a.Store.DB, "another-account")
	if err != nil || len(protected) != 0 {
		t.Fatalf("another account inherited transfer protection: %v, %v", protected, err)
	}
}

func TestConcurrentScanFailureDoesNotEraseTransferState(t *testing.T) {
	for _, duringScan := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "new"}[duringScan], func(t *testing.T) {
			a, f, parent, file, other := scanProtectionFixture(t)
			reserve := func() error {
				_, err := a.Store.NewJob("a", "teldrive_upload", "upload", JobData{NodeIDs: []string{file.ID}})
				return err
			}
			if !duringScan {
				if err := reserve(); err != nil {
					t.Fatal(err)
				}
			}
			for _, id := range []string{parent.ID, file.ID} {
				if err := a.Store.State("a", id, "pending"); err != nil {
					t.Fatal(err)
				}
			}
			failure := errors.New("interrupted directory listing")
			c := &scanHookDrive{fakeDrive: f, onList: func(_ string, page pikpak.Page) (pikpak.Page, error) {
				if duringScan {
					if err := reserve(); err != nil {
						return page, err
					}
				}
				return page, failure
			}}
			if err := a.checkLibrary(context.Background(), c, &Job{AccountID: "a"}); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			assertScanState(t, a, file.ID, "pending")
			assertScanState(t, a, parent.ID, "pending")
			assertScanState(t, a, other.ID, "unknown")
		})
	}
}

func TestConcurrentScanSerializesFinalAlignment(t *testing.T) {
	a, f, parent, file, _ := scanProtectionFixture(t)
	captured := make(chan struct{})
	release := make(chan struct{})
	scanDone := make(chan error, 1)
	alignDone := make(chan error, 1)
	c := &scanHookDrive{fakeDrive: f}
	c.onList = func(id string, page pikpak.Page) (pikpak.Page, error) {
		if id == parent.RemoteID {
			// Upstream's directory listing still shows the upload as pending,
			// while its subsequently returned final file is already complete.
			for i := range page.Files {
				if page.Files[i].ID == file.RemoteID {
					page.Files[i].Phase = "PHASE_TYPE_RUNNING"
				}
			}
			close(captured)
			<-release
		}
		return page, nil
	}
	go func() { scanDone <- a.checkLibrary(context.Background(), c, &Job{AccountID: "a"}) }()
	select {
	case <-captured:
	case <-time.After(3 * time.Second):
		t.Fatal("scan did not reach the controlled listing")
	}
	finalFile := f.files[file.RemoteID]
	go func() { alignDone <- a.align(context.Background(), f, "a", file, finalFile, parent.RemoteID) }()
	select {
	case err := <-alignDone:
		close(release)
		<-scanDone
		t.Fatalf("alignment bypassed in-flight scan: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-scanDone; err != nil {
		t.Fatal(err)
	}
	if err := <-alignDone; err != nil {
		t.Fatal(err)
	}
	assertScanState(t, a, file.ID, "present")
}
