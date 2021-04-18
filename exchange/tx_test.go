package exchange

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	files "github.com/ipfs/go-ipfs-files"
	keystore "github.com/ipfs/go-ipfs-keystore"
	"github.com/ipfs/go-path"
	mocknet "github.com/libp2p/go-libp2p/p2p/net/mock"
	"github.com/myelnet/pop/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestTx(t *testing.T) {
	newNode := func(ctx context.Context, mn mocknet.Mocknet) (*Exchange, *testutil.TestNode) {
		n := testutil.NewTestNode(mn, t)
		opts := Options{
			RepoPath: n.DTTmpDir,
			Keystore: keystore.NewMemKeystore(),
		}
		exch, err := New(ctx, n.Host, n.Ds, opts)
		require.NoError(t, err)
		return exch, n
	}
	// Iterating a ton helps weed out false positives
	for i := 0; i < 1; i++ {
		t.Run(fmt.Sprintf("Try %v", i), func(t *testing.T) {
			bgCtx := context.Background()

			ctx, cancel := context.WithTimeout(bgCtx, 10*time.Second)
			defer cancel()

			mn := mocknet.New(bgCtx)

			var providers []*Exchange
			var pnodes []*testutil.TestNode

			for i := 0; i < 11; i++ {
				exch, n := newNode(ctx, mn)
				providers = append(providers, exch)
				pnodes = append(pnodes, n)
			}
			require.NoError(t, mn.LinkAll())
			require.NoError(t, mn.ConnectAllButSelf())

			// The peer manager has time to fill up while we load this file
			fname := pnodes[0].CreateRandomFile(t, 56000)
			tx := providers[0].Tx(ctx)
			require.NoError(t, tx.PutFile(fname))

			file, err := tx.GetFile(KeyFromPath(fname))
			require.NoError(t, err)
			size, err := file.Size()
			require.NoError(t, err)
			require.Equal(t, size, int64(56000))

			// Commit the transaction will dipatch the content to the network
			require.NoError(t, tx.Commit())

			var records []PRecord
			tx.WatchDispatch(func(rec PRecord) {
				records = append(records, rec)
			})
			require.Equal(t, 7, len(records))
			root := tx.Root()
			tx.Close()

			// Create a new client
			client, _ := newNode(ctx, mn)

			require.NoError(t, mn.LinkAll())
			require.NoError(t, mn.ConnectAllButSelf())

			tx = client.Tx(ctx, WithRoot(root), WithStrategy(SelectFirst))
			require.NoError(t, tx.Query(KeyFromPath(fname)))
			select {
			case <-ctx.Done():
				t.Fatal("tx timeout")
			case <-tx.Done():
			}
			file, err = tx.GetFile(KeyFromPath(fname))
			require.NoError(t, err)
			size, err = file.Size()
			require.NoError(t, err)
			require.Equal(t, size, int64(56000))
		})
	}

}
func genTestFiles(t *testing.T) (map[string]string, []string) {
	dir := t.TempDir()

	testInputs := map[string]string{
		"line1.txt": "Two roads diverged in a yellow wood,\n",
		"line2.txt": "And sorry I could not travel both\n",
		"line3.txt": "And be one traveler, long I stood\n",
		"line4.txt": "And looked down one as far as I could\n",
		"line5.txt": "To where it bent in the undergrowth;\n",
		"line6.txt": "Then took the other, as just as fair,\n",
		"line7.txt": "And having perhaps the better claim,\n",
		"line8.txt": "Because it was grassy and wanted wear;\n",
	}

	paths := make([]string, 0, len(testInputs))

	for p, c := range testInputs {
		path := filepath.Join(dir, p)

		if err := ioutil.WriteFile(path, []byte(c), 0666); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return testInputs, paths
}

func TestTxPutGet(t *testing.T) {
	ctx := context.Background()
	mn := mocknet.New(ctx)

	n := testutil.NewTestNode(mn, t)
	opts := Options{
		RepoPath: n.DTTmpDir,
		Keystore: keystore.NewMemKeystore(),
	}
	exch, err := New(ctx, n.Host, n.Ds, opts)
	require.NoError(t, err)

	filevals, filepaths := genTestFiles(t)

	tx := exch.Tx(ctx)
	sID := tx.StoreID()
	for _, p := range filepaths {
		require.NoError(t, tx.PutFile(p))
	}

	status, err := tx.Status()
	require.NoError(t, err)
	require.Equal(t, len(filepaths), len(status))

	require.NoError(t, tx.Commit())
	r := tx.Root()

	tx = exch.Tx(ctx)
	require.NoError(t, tx.PutFile(filepaths[0]))

	// We should have a new store with a single entry
	status, err = tx.Status()
	require.NoError(t, err)
	require.Equal(t, 1, len(status))

	require.NotEqual(t, sID, tx.StoreID())

	// Test that we can retrieve local content stored by a previous transaction
	tx = exch.Tx(ctx, WithRoot(r))
	for k, v := range filevals {
		nd, err := tx.GetFile(k)
		require.NoError(t, err)
		f := nd.(files.File)
		bytes, err := io.ReadAll(f)
		require.NoError(t, err)

		require.Equal(t, bytes, []byte(v))
	}
	// Generate a path to look for
	p := fmt.Sprintf("/%s/line1.txt", r.String())
	pp := path.FromString(p)
	root, segs, err := path.SplitAbsPath(pp)
	require.NoError(t, err)
	require.Equal(t, root, r)
	require.Equal(t, segs, []string{"line1.txt"})
}

func BenchmarkAdd(b *testing.B) {

	ctx := context.Background()
	mn := mocknet.New(ctx)
	n := testutil.NewTestNode(mn, b)
	opts := Options{
		RepoPath: n.DTTmpDir,
		Keystore: keystore.NewMemKeystore(),
	}
	exch, err := New(ctx, n.Host, n.Ds, opts)
	require.NoError(b, err)

	var filepaths []string
	for i := 0; i < b.N; i++ {
		filepaths = append(filepaths, n.CreateRandomFile(b, 256000))
	}

	b.ReportAllocs()
	b.ResetTimer()
	runtime.GC()

	tx := exch.Tx(ctx)
	for i := 0; i < b.N; i++ {
		require.NoError(b, tx.PutFile(filepaths[i]))
	}
}

// Testing this with race flag to detect any weirdness
func TestTxRace(t *testing.T) {
	ctx := context.Background()
	mn := mocknet.New(ctx)
	n := testutil.NewTestNode(mn, t)
	opts := Options{
		RepoPath: n.DTTmpDir,
		Keystore: keystore.NewMemKeystore(),
	}
	exch, err := New(ctx, n.Host, n.Ds, opts)
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			harness := &testutil.TestNode{}
			tx := exch.Tx(ctx)
			fname1 := harness.CreateRandomFile(t, 100000)
			require.NoError(t, tx.PutFile(fname1))

			fname2 := harness.CreateRandomFile(t, 156000)
			require.NoError(t, tx.PutFile(fname2))

			require.NoError(t, tx.Commit())
		}()
	}
	wg.Wait()
}
