package legs_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/filecoin-project/go-legs"
	"github.com/filecoin-project/go-legs/dtsync"
	"github.com/filecoin-project/go-legs/test"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	"github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multiaddr"
)

func TestLatestSyncSuccess(t *testing.T) {
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	srcHost := test.MkTestHost()
	srcLnkS := test.MkLinkSystem(srcStore)

	dstHost := test.MkTestHost()
	srcHost.Peerstore().AddAddrs(dstHost.ID(), dstHost.Addrs(), time.Hour)
	dstHost.Peerstore().AddAddrs(srcHost.ID(), srcHost.Addrs(), time.Hour)
	dstLnkS := test.MkLinkSystem(dstStore)
	defer srcHost.Close()
	defer dstHost.Close()

	topics := test.WaitForMeshWithMessage(t, testTopic, srcHost, dstHost)

	pub, err := dtsync.NewPublisher(srcHost, srcStore, srcLnkS, testTopic, dtsync.Topic(topics[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	sub, err := legs.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic, nil, legs.Topic(topics[1]))
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	// Store the whole chain in source node
	chainLnks := test.MkChain(srcLnkS, true)

	err = newUpdateTest(pub, sub, dstStore, watcher, srcHost.ID(), chainLnks[2], false, chainLnks[2].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
	err = newUpdateTest(pub, sub, dstStore, watcher, srcHost.ID(), chainLnks[1], false, chainLnks[1].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
	err = newUpdateTest(pub, sub, dstStore, watcher, srcHost.ID(), chainLnks[0], false, chainLnks[0].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSyncFn(t *testing.T) {
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	srcHost := test.MkTestHost()
	srcLnkS := test.MkLinkSystem(srcStore)

	dstHost := test.MkTestHost()
	srcHost.Peerstore().AddAddrs(dstHost.ID(), dstHost.Addrs(), time.Hour)
	dstHost.Peerstore().AddAddrs(srcHost.ID(), srcHost.Addrs(), time.Hour)
	dstLnkS := test.MkLinkSystem(dstStore)
	defer srcHost.Close()
	defer dstHost.Close()

	topics := test.WaitForMeshWithMessage(t, testTopic, srcHost, dstHost)

	pub, err := dtsync.NewPublisher(srcHost, srcStore, srcLnkS, testTopic, dtsync.Topic(topics[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	var blockHookCalls int
	blocksSeenByHook := make(map[cid.Cid]struct{})
	blockHook := func(_ peer.ID, c cid.Cid, _ legs.SegmentSyncActions) {
		blockHookCalls++
		blocksSeenByHook[c] = struct{}{}
	}

	sub, err := legs.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic, nil, legs.Topic(topics[1]), legs.BlockHook(blockHook))
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	if err := srcHost.Connect(context.Background(), dstHost.Peerstore().PeerInfo(dstHost.ID())); err != nil {
		t.Fatal(err)
	}

	// Store the whole chain in source node
	chainLnks := test.MkChain(srcLnkS, true)

	watcher, cancelWatcher := sub.OnSyncFinished()
	defer cancelWatcher()

	// Try to sync with a non-existing cid to check that sync returns with err,
	// and SyncFinished watcher does not get event.
	cids, _ := test.RandomCids(1)
	ctx, syncncl := context.WithTimeout(context.Background(), updateTimeout)
	defer syncncl()
	_, err = sub.Sync(ctx, srcHost.ID(), cids[0], nil, nil)
	if err == nil {
		t.Fatal("expected error when no content to sync")
	}
	syncncl()

	select {
	case <-time.After(updateTimeout):
	case <-watcher:
		t.Fatal("watcher should not receive event if sync error")
	}

	lnk := chainLnks[1]

	// Sync with publisher without publisher publishing to gossipsub channel.
	ctx, syncncl = context.WithTimeout(context.Background(), updateTimeout)
	defer syncncl()
	syncCid, err := sub.Sync(ctx, srcHost.ID(), lnk.(cidlink.Link).Cid, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !syncCid.Equals(lnk.(cidlink.Link).Cid) {
		t.Fatalf("sync'd cid unexpected %s vs %s", syncCid, lnk)
	}
	if _, err := dstStore.Get(context.Background(), datastore.NewKey(syncCid.String())); err != nil {
		t.Fatalf("data not in receiver store: %v", err)
	}
	syncncl()

	_, ok := blocksSeenByHook[lnk.(cidlink.Link).Cid]
	if !ok {
		t.Fatal("block hook did not see link cid")
	}

	if blockHookCalls != 11 {
		t.Fatalf("expected 11 block hook calls, got %d", blockHookCalls)
	}

	// Assert the latestSync is not updated by explicit sync when cid is set
	if sub.GetLatestSync(srcHost.ID()) != nil {
		t.Fatal("Sync should not update latestSync")
	}

	// Assert the latestSync is updated by explicit sync when cid and selector are unset.
	newHead := chainLnks[0].(cidlink.Link).Cid
	if err := pub.UpdateRoot(context.Background(), newHead); err != nil {
		t.Fatal(err)
	}

	select {
	case <-time.After(updateTimeout):
		t.Fatal("timed out waiting for sync from published update")
	case syncFin, open := <-watcher:
		if !open {
			t.Fatal("sync finished channel closed with no event")
		}
		if syncFin.Cid != newHead {
			t.Fatalf("Should have been updated to %s, got %s", newHead, syncFin.Cid)
		}
	}
	cancelWatcher()

	ctx, syncncl = context.WithTimeout(context.Background(), updateTimeout)
	defer syncncl()
	syncCid, err = sub.Sync(ctx, srcHost.ID(), cid.Undef, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !syncCid.Equals(newHead) {
		t.Fatalf("sync'd cid unexpected %s vs %s", syncCid, lnk)
	}
	if _, err := dstStore.Get(context.Background(), datastore.NewKey(syncCid.String())); err != nil {
		t.Fatalf("data not in receiver store: %v", err)
	}
	syncncl()

	err = assertLatestSyncEquals(sub, srcHost.ID(), newHead)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPartialSync(t *testing.T) {
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	testStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	srcHost := test.MkTestHost()
	srcLnkS := test.MkLinkSystem(srcStore)
	testLnkS := test.MkLinkSystem(testStore)

	chainLnks := test.MkChain(testLnkS, true)

	dstHost := test.MkTestHost()
	srcHost.Peerstore().AddAddrs(dstHost.ID(), dstHost.Addrs(), time.Hour)
	dstHost.Peerstore().AddAddrs(srcHost.ID(), srcHost.Addrs(), time.Hour)
	dstLnkS := test.MkLinkSystem(dstStore)

	defer srcHost.Close()
	defer dstHost.Close()

	topics := test.WaitForMeshWithMessage(t, testTopic, srcHost, dstHost)

	pub, err := dtsync.NewPublisher(srcHost, srcStore, srcLnkS, testTopic, dtsync.Topic(topics[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	test.MkChain(srcLnkS, true)

	sub, err := legs.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic, nil, legs.Topic(topics[1]))
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	err = sub.SetLatestSync(srcHost.ID(), chainLnks[3].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}

	if err := srcHost.Connect(context.Background(), dstHost.Peerstore().PeerInfo(dstHost.ID())); err != nil {
		t.Fatal(err)
	}

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	// Fetching first few nodes.
	err = newUpdateTest(pub, sub, dstStore, watcher, srcHost.ID(), chainLnks[2], false, chainLnks[2].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}

	// Check that first nodes hadn't been synced
	if _, err := dstStore.Get(context.Background(), datastore.NewKey(chainLnks[3].(cidlink.Link).Cid.String())); err != datastore.ErrNotFound {
		t.Fatalf("data should not be in receiver store: %v", err)
	}

	// Set latest sync so we pass through one of the links
	err = sub.SetLatestSync(srcHost.ID(), chainLnks[1].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
	err = assertLatestSyncEquals(sub, srcHost.ID(), chainLnks[1].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}

	// Update all the chain from scratch again.
	err = newUpdateTest(pub, sub, dstStore, watcher, srcHost.ID(), chainLnks[0], false, chainLnks[0].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}

	// Check if the node we pass through was retrieved
	if _, err := dstStore.Get(context.Background(), datastore.NewKey(chainLnks[1].(cidlink.Link).Cid.String())); err != datastore.ErrNotFound {
		t.Fatalf("data should not be in receiver store: %v", err)
	}
}

func TestStepByStepSync(t *testing.T) {
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	srcLnkS := test.MkLinkSystem(srcStore)

	srcHost := test.MkTestHost()
	dstHost := test.MkTestHost()
	defer srcHost.Close()
	defer dstHost.Close()

	topics := test.WaitForMeshWithMessage(t, testTopic, srcHost, dstHost)

	dstLnkS := test.MkLinkSystem(dstStore)

	pub, err := dtsync.NewPublisher(srcHost, srcStore, srcLnkS, testTopic, dtsync.Topic(topics[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	sub, err := legs.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic, nil, legs.Topic(topics[1]))
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	// Store the whole chain in source node
	chainLnks := test.MkChain(srcLnkS, true)

	// Store half of the chain already in destination
	// to simulate the partial sync.
	test.MkChain(dstLnkS, true)

	// Sync the rest of the chain
	err = newUpdateTest(pub, sub, dstStore, watcher, srcHost.ID(), chainLnks[1], false, chainLnks[1].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
	err = newUpdateTest(pub, sub, dstStore, watcher, srcHost.ID(), chainLnks[0], false, chainLnks[0].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLatestSyncFailure(t *testing.T) {
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	srcHost := test.MkTestHost()
	defer srcHost.Close()
	srcLnkS := test.MkLinkSystem(srcStore)
	pub, err := dtsync.NewPublisher(srcHost, srcStore, srcLnkS, testTopic)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	chainLnks := test.MkChain(srcLnkS, true)

	dstHost := test.MkTestHost()
	defer dstHost.Close()
	srcHost.Peerstore().AddAddrs(dstHost.ID(), dstHost.Addrs(), time.Hour)
	dstHost.Peerstore().AddAddrs(srcHost.ID(), srcHost.Addrs(), time.Hour)
	dstLnkS := test.MkLinkSystem(dstStore)

	t.Log("source host:", srcHost.ID())
	t.Log("targer host:", dstHost.ID())

	sub, err := legs.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	if err := srcHost.Connect(context.Background(), dstHost.Peerstore().PeerInfo(dstHost.ID())); err != nil {
		t.Fatal(err)
	}

	err = sub.SetLatestSync(srcHost.ID(), chainLnks[3].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	t.Log("Testing sync fail when the other end does not have the data")
	err = newUpdateTest(pub, sub, dstStore, watcher, srcHost.ID(), cidlink.Link{Cid: cid.Undef}, true, chainLnks[3].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
	cncl()
	sub.Close()

	dstStore = dssync.MutexWrap(datastore.NewMapDatastore())
	sub, err = legs.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	err = sub.SetLatestSync(srcHost.ID(), chainLnks[3].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
	watcher, cncl = sub.OnSyncFinished()
	defer cncl()

	t.Log("Testing sync fail when not able to run the full exchange")
	err = newUpdateTest(pub, sub, dstStore, watcher, srcHost.ID(), chainLnks[2], true, chainLnks[3].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAnnounce(t *testing.T) {
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	srcHost := test.MkTestHost()
	srcLnkS := test.MkLinkSystem(srcStore)
	dstHost := test.MkTestHost()
	defer srcHost.Close()
	defer dstHost.Close()

	srcHost.Peerstore().AddAddrs(dstHost.ID(), dstHost.Addrs(), time.Hour)
	dstHost.Peerstore().AddAddrs(srcHost.ID(), srcHost.Addrs(), time.Hour)
	dstLnkS := test.MkLinkSystem(dstStore)

	pub, err := dtsync.NewPublisher(srcHost, srcStore, srcLnkS, testTopic)
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	sub, err := legs.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	// Store the whole chain in source node
	chainLnks := test.MkChain(srcLnkS, true)

	err = newAnnounceTest(pub, sub, dstStore, watcher, srcHost.ID(), srcHost.Addrs(), chainLnks[2], chainLnks[2].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
	err = newAnnounceTest(pub, sub, dstStore, watcher, srcHost.ID(), srcHost.Addrs(), chainLnks[1], chainLnks[1].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
	err = newAnnounceTest(pub, sub, dstStore, watcher, srcHost.ID(), srcHost.Addrs(), chainLnks[0], chainLnks[0].(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}
}

func newAnnounceTest(pub legs.Publisher, sub *legs.Subscriber, dstStore datastore.Batching, watcher <-chan legs.SyncFinished, peerID peer.ID, peerAddrs []multiaddr.Multiaddr, lnk ipld.Link, expectedSync cid.Cid) error {
	var err error
	c := lnk.(cidlink.Link).Cid
	if c != cid.Undef {
		err = pub.SetRoot(context.Background(), c)
		if err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = sub.Announce(ctx, c, peerID, peerAddrs)
	if err != nil {
		return err
	}

	select {
	case <-time.After(updateTimeout):
		return errors.New("timed out waiting for sync to propagate")
	case downstream, open := <-watcher:
		if !open {
			return errors.New("event channle closed without receiving event")
		}
		if !downstream.Cid.Equals(c) {
			return fmt.Errorf("sync returned unexpected cid %s, expected %s", downstream.Cid, c)
		}
		if _, err = dstStore.Get(context.Background(), datastore.NewKey(downstream.Cid.String())); err != nil {
			return fmt.Errorf("data not in receiver store: %s", err)
		}
	}

	return assertLatestSyncEquals(sub, peerID, expectedSync)
}

func newUpdateTest(pub legs.Publisher, sub *legs.Subscriber, dstStore datastore.Batching, watcher <-chan legs.SyncFinished, peerID peer.ID, lnk ipld.Link, withFailure bool, expectedSync cid.Cid) error {
	var err error
	c := lnk.(cidlink.Link).Cid
	if c != cid.Undef {
		err = pub.UpdateRoot(context.Background(), c)
		if err != nil {
			return err
		}
	}

	// If failure. then latestSync should not be updated.
	if withFailure {
		select {
		case <-time.After(3 * time.Second):
		case changeEvent, open := <-watcher:
			if !open {
				return nil
			}
			return fmt.Errorf("no exchange should have been performed, but got change from peer %s for cid %s", changeEvent.PeerID, changeEvent.Cid)
		}
	} else {
		select {
		case <-time.After(updateTimeout):
			return errors.New("timed out waiting for sync to propagate")
		case downstream, open := <-watcher:
			if !open {
				return errors.New("event channle closed without receiving event")
			}
			if !downstream.Cid.Equals(c) {
				return fmt.Errorf("sync returned unexpected cid %s, expected %s", downstream.Cid, c)
			}
			if _, err = dstStore.Get(context.Background(), datastore.NewKey(downstream.Cid.String())); err != nil {
				return fmt.Errorf("data not in receiver store: %s", err)
			}
		}
	}
	return assertLatestSyncEquals(sub, peerID, expectedSync)
}

func assertLatestSyncEquals(sub *legs.Subscriber, peerID peer.ID, want cid.Cid) error {
	latest := sub.GetLatestSync(peerID)
	if latest == nil {
		return errors.New("latest sync is nil")
	}
	got := latest.(cidlink.Link)
	if got.Cid != want {
		return fmt.Errorf("latestSync not updated correctly, got %s want %s", got, want)
	}
	return nil
}
