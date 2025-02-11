package legs_test

import (
	"context"
	cryptorand "crypto/rand"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
	"time"

	"github.com/filecoin-project/go-legs"
	"github.com/filecoin-project/go-legs/dtsync"
	"github.com/filecoin-project/go-legs/httpsync"
	"github.com/filecoin-project/go-legs/test"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	"github.com/ipld/go-ipld-prime"
	"github.com/ipld/go-ipld-prime/datamodel"
	"github.com/ipld/go-ipld-prime/linking"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	basicnode "github.com/ipld/go-ipld-prime/node/basic"
	"github.com/ipld/go-ipld-prime/traversal/selector"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multicodec"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

const (
	testTopic     = "/legs/testtopic"
	updateTimeout = 1 * time.Second
)

type pubMeta struct {
	pub legs.Publisher
	h   host.Host
}

func TestScopedBlockHook(t *testing.T) {
	err := quick.Check(func(ll llBuilder) bool {
		return t.Run("Quickcheck", func(t *testing.T) {
			ds := dssync.MutexWrap(datastore.NewMapDatastore())
			pubHost := test.MkTestHost()
			lsys := test.MkLinkSystem(ds)
			pub, err := dtsync.NewPublisher(pubHost, ds, lsys, testTopic)
			if err != nil {
				t.Fatal(err)
			}

			head := ll.Build(t, lsys)
			if head == nil {
				// We built an empty list. So nothing to test.
				return
			}

			err = pub.UpdateRoot(context.Background(), head.(cidlink.Link).Cid)
			if err != nil {
				t.Fatal(err)
			}

			subDS := dssync.MutexWrap(datastore.NewMapDatastore())
			subLsys := test.MkLinkSystem(subDS)
			subHost := test.MkTestHost()

			var calledGeneralBlockHookTimes int64
			sub, err := legs.NewSubscriber(subHost, subDS, subLsys, testTopic, nil, legs.BlockHook(func(i peer.ID, c cid.Cid, _ legs.SegmentSyncActions) {
				atomic.AddInt64(&calledGeneralBlockHookTimes, 1)
			}))
			if err != nil {
				t.Fatal(err)
			}

			var calledScopedBlockHookTimes int64
			_, err = sub.Sync(context.Background(), pubHost.ID(), cid.Undef, nil, pubHost.Addrs()[0], legs.ScopedBlockHook(func(i peer.ID, c cid.Cid, _ legs.SegmentSyncActions) {
				atomic.AddInt64(&calledScopedBlockHookTimes, 1)
			}))
			if err != nil {
				t.Fatal(err)
			}

			if atomic.LoadInt64(&calledGeneralBlockHookTimes) != 0 {
				t.Fatalf("General block hook should not have been called when scoped block hook is set")
			}
			if atomic.LoadInt64(&calledScopedBlockHookTimes) != int64(ll.Length) {
				t.Fatalf("Didn't call scoped block hook enough times")
			}

			anotherLL := llBuilder{
				Length: ll.Length,
				Seed:   ll.Seed + 1,
			}.Build(t, lsys)

			err = pub.UpdateRoot(context.Background(), anotherLL.(cidlink.Link).Cid)
			if err != nil {
				t.Fatal(err)
			}
			_, err = sub.Sync(context.Background(), pubHost.ID(), cid.Undef, nil, pubHost.Addrs()[0])
			if err != nil {
				t.Fatal(err)
			}

			if atomic.LoadInt64(&calledGeneralBlockHookTimes) != int64(ll.Length) {
				t.Fatalf("General hook should have been called only in secod sync")
			}

		})
	}, &quick.Config{
		MaxCount: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSyncedCidsReturned(t *testing.T) {
	err := quick.Check(func(ll llBuilder) bool {
		return t.Run("Quickcheck", func(t *testing.T) {
			ds := dssync.MutexWrap(datastore.NewMapDatastore())
			pubHost := test.MkTestHost()
			lsys := test.MkLinkSystem(ds)
			pub, err := dtsync.NewPublisher(pubHost, ds, lsys, testTopic)
			if err != nil {
				t.Fatal(err)
			}

			head := ll.Build(t, lsys)
			if head == nil {
				// We built an empty list. So nothing to test.
				return
			}

			err = pub.UpdateRoot(context.Background(), head.(cidlink.Link).Cid)
			if err != nil {
				t.Fatal(err)
			}

			subDS := dssync.MutexWrap(datastore.NewMapDatastore())
			subLsys := test.MkLinkSystem(subDS)
			subHost := test.MkTestHost()

			sub, err := legs.NewSubscriber(subHost, subDS, subLsys, testTopic, nil)
			if err != nil {
				t.Fatal(err)
			}

			onFinished, cancel := sub.OnSyncFinished()
			defer cancel()
			_, err = sub.Sync(context.Background(), pubHost.ID(), cid.Undef, nil, pubHost.Addrs()[0])
			if err != nil {
				t.Fatal(err)
			}

			finishedVal := <-onFinished
			if len(finishedVal.SyncedCids) != int(ll.Length) {
				t.Fatalf("The finished value should include %d synced cids, but has %d", ll.Length, len(finishedVal.SyncedCids))
			}

			if finishedVal.SyncedCids[0] != head.(cidlink.Link).Cid {
				t.Fatal("The latest synced cid should be the head and first in the list")
			}
		})
	}, &quick.Config{
		MaxCount: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSync(t *testing.T) {
	err := quick.Check(func(ll llBuilder, publisherCount uint8) bool {
		return t.Run("Quickcheck", func(t *testing.T) {
			if publisherCount == 0 {
				// Empty case
				return
			}

			var publishers []pubMeta

			// limit to at most 10 concurrent publishers
			publisherCount := int(publisherCount)%10 + 1

			for i := 0; i < publisherCount; i++ {
				ds := dssync.MutexWrap(datastore.NewMapDatastore())
				pubHost := test.MkTestHost()
				lsys := test.MkLinkSystem(ds)
				pub, err := dtsync.NewPublisher(pubHost, ds, lsys, testTopic)
				if err != nil {
					t.Fatal(err)
				}
				publishers = append(publishers, pubMeta{pub, pubHost})

				head := ll.Build(t, lsys)
				if head == nil {
					// We built an empty list. So nothing to test.
					return
				}

				err = pub.UpdateRoot(context.Background(), head.(cidlink.Link).Cid)
				if err != nil {
					t.Fatal(err)
				}
			}

			subDS := dssync.MutexWrap(datastore.NewMapDatastore())
			subLsys := test.MkLinkSystem(subDS)
			subHost := test.MkTestHost()

			var calledTimes int64
			sub, err := legs.NewSubscriber(subHost, subDS, subLsys, testTopic, nil, legs.BlockHook(func(i peer.ID, c cid.Cid, _ legs.SegmentSyncActions) {
				atomic.AddInt64(&calledTimes, 1)
			}))
			if err != nil {
				t.Fatal(err)
			}

			wg := sync.WaitGroup{}
			// Now sync again. We shouldn't call the hook because we persisted our latestSync
			for _, pub := range publishers {
				wg.Add(1)

				go func(pub pubMeta) {
					defer wg.Done()
					_, err := sub.Sync(context.Background(), pub.h.ID(), cid.Undef, nil, pub.h.Addrs()[0])
					if err != nil {
						panic("sync failed")
					}
				}(pub)

			}

			doneChan := make(chan struct{})

			go func() {
				wg.Wait()
				close(doneChan)
			}()

			select {
			case <-time.After(10 * time.Second):
				t.Fatal("Failed to sync")
			case <-doneChan:
			}

			if atomic.LoadInt64(&calledTimes) != int64(ll.Length)*int64(publisherCount) {
				t.Fatalf("Didn't call block hook for each publisher. Expected %d saw %d", int(ll.Length)*publisherCount, calledTimes)
			}
		})
	}, &quick.Config{
		MaxCount: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSync(t *testing.T) {
	err := quick.Check(func(lpsb legsPubSubBuilder, ll llBuilder) bool {
		return t.Run("Quickcheck", func(t *testing.T) {
			pubSys := newHostSystem(t)
			subSys := newHostSystem(t)
			defer pubSys.close()
			defer subSys.close()

			calledTimes := 0
			pubAddr, pub, sub := lpsb.Build(t, testTopic, pubSys, subSys,
				[]legs.Option{legs.BlockHook(func(i peer.ID, c cid.Cid, _ legs.SegmentSyncActions) {
					calledTimes++
				})},
			)

			head := ll.Build(t, pubSys.lsys)
			if head == nil {
				// We built an empty list. So nothing to test.
				return
			}

			err := pub.UpdateRoot(context.Background(), head.(cidlink.Link).Cid)
			if err != nil {
				t.Fatal(err)
			}

			_, err = sub.Sync(context.Background(), pubSys.host.ID(), cid.Undef, nil, pubAddr)
			if err != nil {
				t.Fatal(err)
			}
			calledTimesFirstSync := calledTimes
			latestSync := sub.GetLatestSync(pubSys.host.ID())
			if latestSync != head {
				t.Fatalf("Subscriber did not persist latest sync")
			}
			// Now sync again. We shouldn't call the hook.
			_, err = sub.Sync(context.Background(), pubSys.host.ID(), cid.Undef, nil, pubAddr)
			if err != nil {
				t.Fatal(err)
			}
			if calledTimesFirstSync != calledTimes {
				t.Fatalf("Subscriber called the block hook multiple times for the same sync. Expected %d, got %d", calledTimesFirstSync, calledTimes)
			}

			require.Equal(t, int(ll.Length), calledTimes, "Subscriber did not call the block hook exactly once for each block")
		})
	}, &quick.Config{
		MaxCount: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSyncWithHydratedDataStore tests what happens if we call sync when the
// subscriber datastore already has the dag.
func TestSyncWithHydratedDataStore(t *testing.T) {
	err := quick.Check(func(lpsb legsPubSubBuilder, ll llBuilder) bool {
		return t.Run("Quickcheck", func(t *testing.T) {
			pubPrivKey, _, err := crypto.GenerateEd25519Key(cryptorand.Reader)
			require.NoError(t, err)

			pubDs := dssync.MutexWrap(datastore.NewMapDatastore())
			pubSys := hostSystem{
				privKey: pubPrivKey,
				host:    test.MkTestHost(libp2p.Identity(pubPrivKey)),
				ds:      pubDs,
				lsys:    test.MkLinkSystem(pubDs),
			}
			subDs := dssync.MutexWrap(datastore.NewMapDatastore())
			subSys := hostSystem{
				host: test.MkTestHost(),
				ds:   subDs,
				lsys: test.MkLinkSystem(subDs),
			}

			calledTimes := 0
			var calledWith []cid.Cid
			pubAddr, pub, sub := lpsb.Build(t, testTopic, pubSys, subSys,
				[]legs.Option{legs.BlockHook(func(i peer.ID, c cid.Cid, _ legs.SegmentSyncActions) {
					calledWith = append(calledWith, c)
					calledTimes++
				})},
			)

			head := ll.Build(t, pubSys.lsys)
			if head == nil {
				// We built an empty list. So nothing to test.
				return
			}

			err = pub.UpdateRoot(context.Background(), head.(cidlink.Link).Cid)
			if err != nil {
				t.Fatal(err)
			}

			// Sync once to hydrate the datastore
			// Note we set the cid we are syncing to so we don't update the latestSync.
			_, err = sub.Sync(context.Background(), pubSys.host.ID(), head.(cidlink.Link).Cid, nil, pubAddr)
			require.NoError(t, err)
			require.Equal(t, int(ll.Length), calledTimes, "Subscriber did not call the block hook exactly once for each block")
			require.Equal(t, head.(cidlink.Link).Cid, calledWith[0], "Subscriber did not call the block hook in the correct order")

			calledTimesFirstSync := calledTimes

			// Now sync again. We might call the hook because we don't have the latestSync persisted.
			_, err = sub.Sync(context.Background(), pubSys.host.ID(), cid.Undef, nil, pubAddr)
			if err != nil {
				t.Fatal(err)
			}
			require.GreaterOrEqual(t, calledTimes, calledTimesFirstSync, "Expected to have called block hook twice. Once for each sync.")
		})
	}, &quick.Config{
		MaxCount: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRoundTripSimple(t *testing.T) {
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

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	// Update root with item
	itm := basicnode.NewString("hello world")
	lnk, err := test.Store(srcStore, itm)
	if err != nil {
		t.Fatal(err)
	}

	if err := pub.UpdateRoot(context.Background(), lnk.(cidlink.Link).Cid); err != nil {
		t.Fatal(err)
	}

	select {
	case <-time.After(updateTimeout):
		t.Fatal("timed out waiting for sync to propagate")
	case downstream := <-watcher:
		if !downstream.Cid.Equals(lnk.(cidlink.Link).Cid) {
			t.Fatalf("sync'd cid unexpected %s vs %s", downstream.Cid, lnk)
		}
		if _, err := dstStore.Get(context.Background(), datastore.NewKey(downstream.Cid.String())); err != nil {
			t.Fatalf("data not in receiver store: %v", err)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	// Init legs publisher and subscriber
	srcStore1 := dssync.MutexWrap(datastore.NewMapDatastore())
	srcHost1 := test.MkTestHost()
	defer srcHost1.Close()
	srcLnkS1 := test.MkLinkSystem(srcStore1)

	srcStore2 := dssync.MutexWrap(datastore.NewMapDatastore())
	srcHost2 := test.MkTestHost()
	defer srcHost2.Close()
	srcLnkS2 := test.MkLinkSystem(srcStore2)

	dstStore := dssync.MutexWrap(datastore.NewMapDatastore())
	dstHost := test.MkTestHost()
	defer dstHost.Close()
	dstLnkS := test.MkLinkSystem(dstStore)

	topics := test.WaitForMeshWithMessage(t, "testTopic", srcHost1, srcHost2, dstHost)

	pub1, err := dtsync.NewPublisher(srcHost1, srcStore1, srcLnkS1, "", dtsync.Topic(topics[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer pub1.Close()

	pub2, err := dtsync.NewPublisher(srcHost2, srcStore2, srcLnkS2, "", dtsync.Topic(topics[1]))
	if err != nil {
		t.Fatal(err)
	}
	defer pub2.Close()

	blocksSeenByHook := make(map[cid.Cid]struct{})
	blockHook := func(p peer.ID, c cid.Cid, _ legs.SegmentSyncActions) {
		blocksSeenByHook[c] = struct{}{}
		t.Log("block hook got", c, "from", p)
	}

	sub, err := legs.NewSubscriber(dstHost, dstStore, dstLnkS, testTopic, nil, legs.Topic(topics[2]), legs.BlockHook(blockHook))
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	watcher1, cncl1 := sub.OnSyncFinished()
	defer cncl1()
	watcher2, cncl2 := sub.OnSyncFinished()
	defer cncl2()

	// Update root on publisher one with item
	itm1 := basicnode.NewString("hello world")
	lnk1, err := test.Store(srcStore1, itm1)
	if err != nil {
		t.Fatal(err)
	}
	// Update root on publisher one with item
	itm2 := basicnode.NewString("hello world 2")
	lnk2, err := test.Store(srcStore2, itm2)
	if err != nil {
		t.Fatal(err)
	}

	if err = pub1.UpdateRoot(context.Background(), lnk1.(cidlink.Link).Cid); err != nil {
		t.Fatal(err)
	}
	t.Log("Publish 1:", lnk1.(cidlink.Link).Cid)
	waitForSync(t, "Watcher 1", dstStore, lnk1.(cidlink.Link), watcher1)
	waitForSync(t, "Watcher 2", dstStore, lnk1.(cidlink.Link), watcher2)

	if err = pub2.UpdateRoot(context.Background(), lnk2.(cidlink.Link).Cid); err != nil {
		t.Fatal(err)
	}
	t.Log("Publish 2:", lnk2.(cidlink.Link).Cid)
	waitForSync(t, "Watcher 1", dstStore, lnk2.(cidlink.Link), watcher1)
	waitForSync(t, "Watcher 2", dstStore, lnk2.(cidlink.Link), watcher2)

	if len(blocksSeenByHook) != 2 {
		t.Fatal("expected 2 blocks seen by hook, got", len(blocksSeenByHook))
	}
	_, ok := blocksSeenByHook[lnk1.(cidlink.Link).Cid]
	if !ok {
		t.Fatal("hook did not see link1")
	}
	_, ok = blocksSeenByHook[lnk2.(cidlink.Link).Cid]
	if !ok {
		t.Fatal("hook did not see link2")
	}
}

func TestHttpPeerAddrPeerstore(t *testing.T) {
	pubHostSys := newHostSystem(t)
	subHostSys := newHostSystem(t)
	defer pubHostSys.close()
	defer subHostSys.close()

	pubAddr, pub, sub := legsPubSubBuilder{
		IsHttp: true,
	}.Build(t, testTopic, pubHostSys, subHostSys, nil)

	ll := llBuilder{
		Length: 3,
		Seed:   1,
	}.Build(t, pubHostSys.lsys)

	// a new link on top of ll
	nextLL := llBuilder{
		Length: 1,
		Seed:   2,
	}.BuildWithPrev(t, pubHostSys.lsys, ll)

	prevHead := ll
	head := nextLL

	err := pub.UpdateRoot(context.Background(), prevHead.(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}

	_, err = sub.Sync(context.Background(), pubHostSys.host.ID(), cid.Undef, nil, pubAddr)
	if err != nil {
		t.Fatal(err)
	}

	err = pub.UpdateRoot(context.Background(), head.(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}

	// Now call sync again with no address. The subscriber should re-use the
	// previous address and succeeed.
	_, err = sub.Sync(context.Background(), pubHostSys.host.ID(), cid.Undef, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

}

func TestRateLimiter(t *testing.T) {
	type testCase struct {
		name   string
		isHttp bool
	}

	testCases := []testCase{
		{"DT rate limiter", false},
		{"HTTP rate limiter", true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			pubHostSys := newHostSystem(t)
			subHostSys := newHostSystem(t)
			defer pubHostSys.close()
			defer subHostSys.close()

			tokenEvery := 100 * time.Millisecond
			limiter := rate.NewLimiter(rate.Every(tokenEvery), 1)
			var calledTimes int64
			pubAddr, pub, sub := legsPubSubBuilder{
				IsHttp: tc.isHttp,
			}.Build(t, testTopic, pubHostSys, subHostSys, []legs.Option{
				legs.BlockHook(func(i peer.ID, c cid.Cid, _ legs.SegmentSyncActions) {
					atomic.AddInt64(&calledTimes, 1)
				}),
				legs.RateLimiter(func(publisher peer.ID) *rate.Limiter {
					return limiter
				}),
			})

			llB := llBuilder{
				Length: 5,
			}
			ll := llB.Build(t, pubHostSys.lsys)

			err := pub.SetRoot(context.Background(), ll.(cidlink.Link).Cid)
			if err != nil {
				t.Fatal(err)
			}

			start := time.Now()
			_, err = sub.Sync(context.Background(), pubHostSys.host.ID(), cid.Undef, nil, pubAddr)
			require.NoError(t, err)
			// Minus 1 because we start with a full bucket.
			require.GreaterOrEqual(t, time.Since(start), tokenEvery*time.Duration(llB.Length-1))

			require.Equal(t, atomic.LoadInt64(&calledTimes), int64(llB.Length))
		})
	}

}

func TestBackpressureDoesntDeadlock(t *testing.T) {
	pubHostSys := newHostSystem(t)
	subHostSys := newHostSystem(t)
	defer pubHostSys.close()
	defer subHostSys.close()

	pubAddr, pub, sub := legsPubSubBuilder{}.Build(t, testTopic, pubHostSys, subHostSys, nil)

	ll := llBuilder{
		Length: 1,
		Seed:   1,
	}.Build(t, pubHostSys.lsys)

	// a new link on top of ll
	nextLL := llBuilder{
		Length: 1,
		Seed:   2,
	}.BuildWithPrev(t, pubHostSys.lsys, ll)

	prevHead := ll
	head := nextLL

	// Purposefully not pulling from this channel yet to create backpressure
	onSyncFinishedChan, cncl := sub.OnSyncFinished()
	defer cncl()

	err := pub.UpdateRoot(context.Background(), prevHead.(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}

	_, err = sub.Sync(context.Background(), pubHostSys.host.ID(), cid.Undef, nil, pubAddr)
	if err != nil {
		t.Fatal(err)
	}

	err = pub.UpdateRoot(context.Background(), head.(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}

	_, err = sub.Sync(context.Background(), pubHostSys.host.ID(), cid.Undef, nil, pubAddr)
	if err != nil {
		t.Fatal(err)
	}

	head = llBuilder{
		Length: 1,
		Seed:   2,
	}.BuildWithPrev(t, pubHostSys.lsys, head)

	err = pub.UpdateRoot(context.Background(), head.(cidlink.Link).Cid)
	if err != nil {
		t.Fatal(err)
	}

	// This is blocked until we read from onSyncFinishedChan
	syncDoneCh := make(chan error)
	go func() {
		_, err = sub.Sync(context.Background(), pubHostSys.host.ID(), cid.Undef, nil, pubAddr)
		syncDoneCh <- err
	}()

	specificSyncDoneCh := make(chan error)
	go func() {
		// A sleep so that the upper sync starts first
		time.Sleep(time.Second)
		sel := legs.ExploreRecursiveWithStopNode(selector.RecursionLimitDepth(1), nil, nil)
		_, err = sub.Sync(context.Background(), pubHostSys.host.ID(), prevHead.(cidlink.Link).Cid, sel, pubAddr)
		specificSyncDoneCh <- err
	}()

	select {
	case <-syncDoneCh:
		t.Fatal("sync should not have finished because it should be blocked by backpressure on the onSyncFinishedChan")
	case err := <-specificSyncDoneCh:
		// This sync should finish because it's not blocked by backpressure. This is
		// because it's a simple sync not trying to setLatest. This is the kind of
		// sync a user will call explicitly, so it should not be blocked by the
		// backpressure of SyncFinishedEvent (it doesn't emit a SyncFinishedEvent).
		require.NoError(t, err)
	}

	// Now pull from onSyncFinishedChan
	select {
	case <-onSyncFinishedChan:
	default:
		t.Fatal("Expected event to be ready to read from onSyncFinishedChan")
	}
	emptySyncFinishedChan(onSyncFinishedChan)

	// Now the syncDoneCh should be able to proceed
	err = <-syncDoneCh
	require.NoError(t, err)

	// So that we can close properly
	emptySyncFinishedChan(onSyncFinishedChan)
}

func emptySyncFinishedChan(ch <-chan legs.SyncFinished) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func waitForSync(t *testing.T, logPrefix string, store *dssync.MutexDatastore, expectedCid cidlink.Link, watcher <-chan legs.SyncFinished) {
	select {
	case <-time.After(updateTimeout):
		t.Fatal("timed out waiting for sync to propogate")
	case downstream := <-watcher:
		if !downstream.Cid.Equals(expectedCid.Cid) {
			t.Fatalf("sync'd cid unexpected %s vs %s", downstream, expectedCid.Cid)
		}
		if _, err := store.Get(context.Background(), datastore.NewKey(downstream.Cid.String())); err != nil {
			t.Fatalf("data not in receiver store: %v", err)
		}
		t.Log(logPrefix+" got sync:", downstream.Cid)
	}

}

func TestCloseSubscriber(t *testing.T) {
	st := dssync.MutexWrap(datastore.NewMapDatastore())
	sh := test.MkTestHost()
	lsys := test.MkLinkSystem(st)

	sub, err := legs.NewSubscriber(sh, st, lsys, testTopic, nil)
	if err != nil {
		t.Fatal(err)
	}

	watcher, cncl := sub.OnSyncFinished()
	defer cncl()

	err = sub.Close()
	if err != nil {
		t.Fatal(err)
	}

	select {
	case _, open := <-watcher:
		if open {
			t.Fatal("Watcher channel should have been closed")
		}
	case <-time.After(updateTimeout):
		t.Fatal("timed out waiting for watcher to close")
	}

	err = sub.Close()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		cncl()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(updateTimeout):
		t.Fatal("OnSyncFinished cancel func did not return after Close")
	}
}

type legsPubSubBuilder struct {
	IsHttp bool
}

type hostSystem struct {
	privKey crypto.PrivKey
	host    host.Host
	ds      datastore.Batching
	lsys    ipld.LinkSystem
}

func newHostSystem(t *testing.T) hostSystem {
	privKey, _, err := crypto.GenerateEd25519Key(cryptorand.Reader)
	require.NoError(t, err)
	ds := dssync.MutexWrap(datastore.NewMapDatastore())
	return hostSystem{
		privKey: privKey,
		host:    test.MkTestHost(libp2p.Identity(privKey)),
		ds:      ds,
		lsys:    test.MkLinkSystem(ds),
	}
}

func (h *hostSystem) close() {
	h.host.Close()
}

func (b legsPubSubBuilder) Build(t *testing.T, topicName string, pubSys hostSystem, subSys hostSystem, subOpts []legs.Option) (multiaddr.Multiaddr, legs.Publisher, *legs.Subscriber) {
	var pubAddr multiaddr.Multiaddr
	var pub legs.Publisher
	var err error
	if b.IsHttp {
		var id peer.ID
		id, err = peer.IDFromPrivateKey(pubSys.privKey)
		if err != nil {
			t.Fatal(err)
		}
		httpPub, err := httpsync.NewPublisher("127.0.0.1:0", pubSys.lsys, id, pubSys.privKey)
		require.NoError(t, err)
		pubAddr = httpPub.Address()
		pub = httpPub
	} else {
		pub, err = dtsync.NewPublisher(pubSys.host, pubSys.ds, pubSys.lsys, topicName)
		pubAddr = pubSys.host.Addrs()[0]
	}
	if err != nil {
		t.Fatal(err)
	}
	sub, err := legs.NewSubscriber(subSys.host, subSys.ds, subSys.lsys, topicName, nil, subOpts...)
	if err != nil {
		t.Fatal(err)
	}

	return pubAddr, pub, sub

}

type llBuilder struct {
	Length uint8
	Seed   int64
}

func (b llBuilder) Build(t *testing.T, lsys ipld.LinkSystem) datamodel.Link {
	return b.BuildWithPrev(t, lsys, nil)
}

func (b llBuilder) BuildWithPrev(t *testing.T, lsys ipld.LinkSystem, prev datamodel.Link) datamodel.Link {
	var linkproto = cidlink.LinkPrototype{
		Prefix: cid.Prefix{
			Version:  1,
			Codec:    uint64(multicodec.DagJson),
			MhType:   uint64(multicodec.Sha2_256),
			MhLength: 16,
		},
	}

	rng := rand.New(rand.NewSource(b.Seed))
	for i := 0; i < int(b.Length); i++ {
		p := basicnode.Prototype.Map
		b := p.NewBuilder()
		ma, err := b.BeginMap(2)
		if err != nil {
			t.Fatal(err)
		}
		eb, err := ma.AssembleEntry("Value")
		if err != nil {
			t.Fatal(err)
		}
		err = eb.AssignInt(int64(rng.Intn(100)))
		if err != nil {
			t.Fatal(err)
		}
		eb, err = ma.AssembleEntry("Next")
		if err != nil {
			t.Fatal(err)
		}
		if prev != nil {
			err = eb.AssignLink(prev)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			err = eb.AssignNull()
			if err != nil {
				t.Fatal(err)
			}
		}
		err = ma.Finish()
		if err != nil {
			t.Fatal(err)
		}

		n := b.Build()

		prev, err = lsys.Store(linking.LinkContext{}, linkproto, n)
		if err != nil {
			t.Fatal(err)
		}
	}

	return prev
}
