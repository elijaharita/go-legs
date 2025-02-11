package legs_test

import (
	"context"
	"log"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/filecoin-project/go-legs"
	"github.com/filecoin-project/go-legs/dtsync"
	"github.com/filecoin-project/go-legs/test"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	_ "github.com/ipld/go-ipld-prime/codec/dagcbor"
	_ "github.com/ipld/go-ipld-prime/codec/dagjson"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
)

func TestMain(m *testing.M) {
	if runtime.GOARCH == "386" {
		log.Println("Skipping tests, cannot use GOARCH=386")
		return
	}

	// Run tests.
	os.Exit(m.Run())
}

func initPubSub(t *testing.T, srcStore, dstStore datastore.Batching) (host.Host, host.Host, legs.Publisher, *legs.Subscriber, error) {
	srcHost := test.MkTestHost()
	dstHost := test.MkTestHost()

	topics := test.WaitForMeshWithMessage(t, testTopic, srcHost, dstHost)

	srcLnkS := test.MkLinkSystem(srcStore)

	pub, err := dtsync.NewPublisher(srcHost, srcStore, srcLnkS, testTopic, dtsync.Topic(topics[0]), dtsync.WithExtraData([]byte("t01000")))
	if err != nil {
		return nil, nil, nil, nil, err
	}

	srcHost.Peerstore().AddAddrs(dstHost.ID(), dstHost.Addrs(), time.Hour)
	dstHost.Peerstore().AddAddrs(srcHost.ID(), srcHost.Addrs(), time.Hour)
	dstLnkS := test.MkLinkSystem(dstStore)

	sub, err := legs.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic, nil, legs.Topic(topics[1]))
	if err != nil {
		return nil, nil, nil, nil, err
	}

	if err := srcHost.Connect(context.Background(), dstHost.Peerstore().PeerInfo(dstHost.ID())); err != nil {
		return nil, nil, nil, nil, err
	}

	return srcHost, dstHost, pub, sub, nil
}

func TestAllowPeerReject(t *testing.T) {
	// Init legs publisher and subscriber
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	srcHost, dstHost, pub, sub, err := initPubSub(t, srcStore, dstStore)
	if err != nil {
		t.Fatal(err)
	}
	defer srcHost.Close()
	defer dstHost.Close()
	defer pub.Close()
	defer sub.Close()

	// Set function to reject anything except dstHost, which is not the one
	// generating the update.
	sub.SetAllowPeer(func(peerID peer.ID) bool {
		return peerID == dstHost.ID()
	})

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	c := mkLnk(t, srcStore)

	// Update root with item
	err = pub.UpdateRoot(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-time.After(3 * time.Second):
	case _, open := <-watcher:
		if open {
			t.Fatal("something was exchanged, and that is wrong")
		}
	}
}

func TestAllowPeerAllows(t *testing.T) {
	// Init legs publisher and subscriber
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	srcHost, dstHost, pub, sub, err := initPubSub(t, srcStore, dstStore)
	if err != nil {
		t.Fatal(err)
	}
	defer srcHost.Close()
	defer dstHost.Close()
	defer pub.Close()
	defer sub.Close()

	// Set function to allow any peer.
	sub.SetAllowPeer(func(_ peer.ID) bool {
		return true
	})

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	c := mkLnk(t, srcStore)

	// Update root with item
	err = pub.UpdateRoot(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-time.After(updateTimeout):
		t.Fatal("timed out waiting for SyncFinished")
	case <-watcher:
	}
}

func TestPublisherRejectsPeer(t *testing.T) {
	// Init legs publisher and subscriber
	srcStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())

	srcHost := test.MkTestHost()
	dstHost := test.MkTestHost()
	defer srcHost.Close()
	defer dstHost.Close()

	topics := test.WaitForMeshWithMessage(t, testTopic, srcHost, dstHost)

	srcLnkS := test.MkLinkSystem(srcStore)

	blockID := dstHost.ID()
	allowPeer := func(peerID peer.ID) bool {
		return peerID != blockID
	}

	pub, err := dtsync.NewPublisher(srcHost, srcStore, srcLnkS, testTopic, dtsync.Topic(topics[0]), dtsync.AllowPeer(allowPeer))
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	srcHost.Peerstore().AddAddrs(dstHost.ID(), dstHost.Addrs(), time.Hour)
	dstHost.Peerstore().AddAddrs(srcHost.ID(), srcHost.Addrs(), time.Hour)
	dstLnkS := test.MkLinkSystem(dstStore)

	sub, err := legs.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic, nil, legs.Topic(topics[1]))
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	if err := srcHost.Connect(context.Background(), dstHost.Peerstore().PeerInfo(dstHost.ID())); err != nil {
		t.Fatal(err)
	}

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	c := mkLnk(t, srcStore)

	// Update root with item
	err = pub.UpdateRoot(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-time.After(updateTimeout):
		t.Log("publisher blocked", blockID)
	case <-watcher:
		t.Fatal("sync should not have happened with blocked ID")
	}

	blockID = peer.ID("")

	c = mkLnk(t, srcStore)

	// Update root with item
	err = pub.UpdateRoot(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-time.After(updateTimeout):
		t.Fatal("timed out waiting for SyncFinished")
	case <-watcher:
		t.Log("synced with allowed ID")
	}
}

func TestIdleHandlerCleaner(t *testing.T) {
	blocksSeenByHook := make(map[cid.Cid]struct{})
	blockHook := func(p peer.ID, c cid.Cid, _ legs.SegmentSyncActions) {
		blocksSeenByHook[c] = struct{}{}
	}

	ttl := time.Second
	te := setupPublisherSubscriber(t, []legs.Option{legs.BlockHook(blockHook), legs.IdleHandlerTTL(ttl)})

	rootLnk, err := test.Store(te.srcStore, basicnode.NewString("hello world"))
	if err != nil {
		t.Fatal(err)
	}
	if err := te.pub.UpdateRoot(context.Background(), rootLnk.(cidlink.Link).Cid); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Do a sync to create the handler.
	_, err = te.sub.Sync(ctx, te.srcHost.ID(), cid.Undef, nil, te.pubAddr)
	if err != nil {
		t.Fatal(err)
	}

	// Check that the handler is preeent by seeing if it can be removed.
	if !te.sub.RemoveHandler(te.srcHost.ID()) {
		t.Fatal("Expected handler to be present")
	}

	// Do another sync to re-create the handler.
	_, err = te.sub.Sync(ctx, te.srcHost.ID(), cid.Undef, nil, te.pubAddr)
	if err != nil {
		t.Fatal(err)
	}

	// For long enough for the idle cleaner to remove the handler, and check
	// that it was removed.
	time.Sleep(3 * ttl)
	if te.sub.RemoveHandler(te.srcHost.ID()) {
		t.Fatal("Expected handler to already be removed")
	}
}

func mkLnk(t *testing.T, srcStore datastore.Batching) cid.Cid {
	// Update root with item
	np := basicnode.Prototype__Any{}
	nb := np.NewBuilder()
	ma, _ := nb.BeginMap(2)
	err := ma.AssembleKey().AssignString("hey")
	if err != nil {
		t.Fatal(err)
	}
	if err = ma.AssembleValue().AssignString("it works!"); err != nil {
		t.Fatal(err)
	}
	if err = ma.AssembleKey().AssignString("yes"); err != nil {
		t.Fatal(err)
	}
	if err = ma.AssembleValue().AssignBool(true); err != nil {
		t.Fatal(err)
	}
	if err = ma.Finish(); err != nil {
		t.Fatal(err)
	}
	n := nb.Build()
	lnk, err := test.Store(srcStore, n)
	if err != nil {
		t.Fatal(err)
	}

	return lnk.(cidlink.Link).Cid
}
