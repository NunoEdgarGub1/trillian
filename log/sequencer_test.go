// Copyright 2016 Google Inc. All Rights Reserved.
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

package log

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/google/trillian"
	"github.com/google/trillian/crypto/keys/pem"
	"github.com/google/trillian/merkle/rfc6962"
	"github.com/google/trillian/quota"
	"github.com/google/trillian/storage"
	"github.com/google/trillian/testonly"
	"github.com/google/trillian/types"
	"github.com/google/trillian/util"

	tcrypto "github.com/google/trillian/crypto"
	stestonly "github.com/google/trillian/storage/testonly"
)

var (
	// These can be shared between tests as they're never modified
	testLeaf16Data    = []byte("testdataforleaf")
	testLeaf16Hash, _ = rfc6962.DefaultHasher.HashLeaf(testLeaf16Data)
	testLeaf16        = &trillian.LogLeaf{
		MerkleLeafHash:     testLeaf16Hash,
		LeafValue:          testLeaf16Data,
		ExtraData:          nil,
		LeafIndex:          16,
		IntegrateTimestamp: testonly.MustToTimestampProto(fakeTime()),
	}

	testRoot16 = &types.LogRootV1{
		TreeSize: 16,
		Revision: 5,
		// RootHash can't be nil because that's how the sequencer currently
		// detects that there was no stored tree head.
		RootHash:       []byte{},
		TimestampNanos: uint64(fakeTimeForTest.Add(-10 * time.Millisecond).UnixNano()),
	}

	fixedSigner         = newSignerWithFixedSig([]byte("signed"))
	testSignedRoot16, _ = tcrypto.NewSigner(0, fixedSigner, crypto.SHA256).SignLogRoot(testRoot16)
	newSignedRoot16, _  = tcrypto.NewSigner(0, fixedSigner, crypto.SHA256).
				SignLogRoot(&types.LogRootV1{
			TimestampNanos: uint64(fakeTimeForTest.UnixNano()),
			TreeSize:       testRoot16.TreeSize,
			Revision:       testRoot16.Revision + 1,
			RootHash:       testRoot16.RootHash,
		})

	testRoot17 = &types.LogRootV1{
		TreeSize: 16,
		Revision: 5,
		// RootHash can't be nil because that's how the sequencer currently
		// detects that there was no stored tree head.
		RootHash:       []byte{},
		TimestampNanos: uint64(fakeTimeForTest.UnixNano()),
	}
	testSignedRoot17, _ = tcrypto.NewSigner(0, fixedSigner, crypto.SHA256).SignLogRoot(testRoot17)

	testRoot18 = &types.LogRootV1{
		TreeSize: 16,
		Revision: 5,
		// RootHash can't be nil because that's how the sequencer currently
		// detects that there was no stored tree head.
		RootHash:       []byte{},
		TimestampNanos: uint64(fakeTimeForTest.Add(10 * time.Millisecond).UnixNano()),
	}
	testSignedRoot18, _ = tcrypto.NewSigner(0, fixedSigner, crypto.SHA256).SignLogRoot(testRoot18)

	// These will be accepted in either order because of custom sorting in the mock
	updatedNodes = []storage.Node{
		{
			NodeID:       storage.NodeID{Path: []uint8{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10}, PrefixLenBits: 64},
			Hash:         testonly.MustDecodeBase64("L5Iyd7aFOVewxiRm29xD+EU+jvEo4RfufBijKdflWMk="),
			NodeRevision: 6,
		},
		{
			NodeID:       storage.NodeID{Path: []uint8{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, PrefixLenBits: 59},
			Hash:         testonly.MustDecodeBase64("R57DrKTGuZdjCNXjv6InGrm4rABLOn9yWpdHmYOoLwU="),
			NodeRevision: 6,
		},
	}

	testRoot = &types.LogRootV1{
		RootHash:       []byte{71, 158, 195, 172, 164, 198, 185, 151, 99, 8, 213, 227, 191, 162, 39, 26, 185, 184, 172, 0, 75, 58, 127, 114, 90, 151, 71, 153, 131, 168, 47, 5},
		TimestampNanos: uint64(fakeTimeForTest.UnixNano()),
		Revision:       6,
		TreeSize:       17,
	}
	testSignedRoot, _ = tcrypto.NewSigner(0, fixedSigner, crypto.SHA256).SignLogRoot(testRoot)
)

var fakeTimeForTest = fakeTime()

// Any tests relying on time should use this fixed value
const fakeTimeStr string = "2016-05-25T10:55:05Z"

// testParameters bundles up values needed for setting mock expectations in tests
type testParameters struct {
	logID  int64
	signer crypto.Signer

	beginFails   bool
	dequeueLimit int

	shouldCommit bool
	commitFails  bool
	commitError  error

	skipDequeue    bool
	dequeuedLeaves []*trillian.LogLeaf
	dequeuedError  error

	latestSignedRootError error
	latestSignedRoot      *trillian.SignedLogRoot

	updatedLeaves      *[]*trillian.LogLeaf
	updatedLeavesError error

	merkleNodesSet      *[]storage.Node
	merkleNodesSetError error

	skipStoreSignedRoot  bool
	storeSignedRoot      *trillian.SignedLogRoot
	storeSignedRootError error

	writeRevision int64

	overrideDequeueTime *time.Time

	// qm is the quota.Manager to be used. If nil, quota.Noop() is used instead.
	qm quota.Manager
}

// Tests get their own mock context so they can be run in parallel safely
type testContext struct {
	mockTx      *storage.MockLogTreeTX
	fakeStorage storage.LogStorage
	signer      *tcrypto.Signer
	sequencer   *Sequencer
}

// This gets modified so tests need their own copies
func getLeaf42() *trillian.LogLeaf {
	return &trillian.LogLeaf{
		MerkleLeafHash: testLeaf16Hash,
		LeafValue:      testLeaf16Data,
		ExtraData:      nil,
		LeafIndex:      42,
	}
}

func fakeTime() time.Time {
	fakeTimeForTest, err := time.Parse(time.RFC3339, fakeTimeStr)

	if err != nil {
		panic(fmt.Sprintf("Test has an invalid fake time: %s", err))
	}

	return fakeTimeForTest
}

func newSignerWithFixedSig(sig []byte) crypto.Signer {
	key, err := pem.UnmarshalPublicKey(testonly.DemoPublicKey)
	if err != nil {
		panic(err)
	}

	return testonly.NewSignerWithFixedSig(key, sig)
}

func newSignerWithErr(signErr error) (crypto.Signer, error) {
	key, err := pem.UnmarshalPublicKey(testonly.DemoPublicKey)
	if err != nil {
		return nil, err
	}

	return testonly.NewSignerWithErr(key, signErr), nil
}

func createTestContext(ctrl *gomock.Controller, params testParameters) (testContext, context.Context) {
	fakeStorage := &stestonly.FakeLogStorage{}
	mockTx := storage.NewMockLogTreeTX(ctrl)

	mockTx.EXPECT().WriteRevision().AnyTimes().Return(params.writeRevision)
	if params.beginFails {
		fakeStorage.TXErr = errors.New("TX")
	} else {
		mockTx.EXPECT().Close()
		fakeStorage.TX = mockTx
	}

	if params.shouldCommit {
		if !params.commitFails {
			mockTx.EXPECT().Commit().Return(nil)
		} else {
			mockTx.EXPECT().Commit().Return(params.commitError)
		}
	}
	// Close is always called, regardless of explicit commits
	mockTx.EXPECT().Close().AnyTimes().Return(nil)

	if !params.skipDequeue {
		if params.overrideDequeueTime != nil {
			mockTx.EXPECT().DequeueLeaves(gomock.Any(), params.dequeueLimit, *params.overrideDequeueTime).Return(params.dequeuedLeaves, params.dequeuedError)
		} else {
			mockTx.EXPECT().DequeueLeaves(gomock.Any(), params.dequeueLimit, fakeTimeForTest).Return(params.dequeuedLeaves, params.dequeuedError)
		}
	}

	if params.latestSignedRoot != nil {
		mockTx.EXPECT().LatestSignedLogRoot(gomock.Any()).Return(*params.latestSignedRoot, params.latestSignedRootError)
	}

	if params.updatedLeaves != nil {
		mockTx.EXPECT().UpdateSequencedLeaves(gomock.Any(), *params.updatedLeaves).Return(params.updatedLeavesError)
	}

	if params.merkleNodesSet != nil {
		mockTx.EXPECT().SetMerkleNodes(gomock.Any(), stestonly.NodeSet(*params.merkleNodesSet)).Return(params.merkleNodesSetError)
	}

	if !params.skipStoreSignedRoot {
		if params.storeSignedRoot != nil {
			mockTx.EXPECT().StoreSignedLogRoot(gomock.Any(), *params.storeSignedRoot).Return(params.storeSignedRootError)
		} else {
			// At the moment if we're going to fail the operation we accept any root
			mockTx.EXPECT().StoreSignedLogRoot(gomock.Any(), gomock.Any()).Return(params.storeSignedRootError)
		}
	}

	signer := tcrypto.NewSigner(0, params.signer, crypto.SHA256)
	qm := params.qm
	if qm == nil {
		qm = quota.Noop()
	}
	sequencer := NewSequencer(rfc6962.DefaultHasher, util.NewFakeTimeSource(fakeTimeForTest), fakeStorage, signer, nil, qm)
	return testContext{mockTx: mockTx, fakeStorage: fakeStorage, signer: signer, sequencer: sequencer}, context.Background()
}

// Tests for sequencer. Currently relies on having a database set up. This might change in future
// as it would be better if it was not tied to a specific storage mechanism.

func TestIntegrateBatch(t *testing.T) {
	signerErr, err := newSignerWithErr(errors.New("signerfailed"))
	if err != nil {
		t.Fatalf("Failed to create test signer (%v)", err)
	}
	leaves16 := []*trillian.LogLeaf{testLeaf16}
	guardWindow := time.Second * 10
	expectedCutoffTime := fakeTimeForTest.Add(-guardWindow)
	noLeaves := []*trillian.LogLeaf{}
	noNodes := []storage.Node{}
	specs := []quota.Spec{
		{Group: quota.Tree, Kind: quota.Read, TreeID: 154035},
		{Group: quota.Tree, Kind: quota.Write, TreeID: 154035},
		{Group: quota.Global, Kind: quota.Read},
		{Group: quota.Global, Kind: quota.Write},
	}

	var tests = []struct {
		desc            string
		params          testParameters
		guardWindow     time.Duration
		maxRootDuration time.Duration
		wantCount       int
		errStr          string
	}{
		{
			desc: "begin-tx-fails",
			params: testParameters{
				logID:               154035,
				beginFails:          true,
				skipDequeue:         true,
				skipStoreSignedRoot: true,
			},
			errStr: "TX",
		},
		{
			desc: "nothing-queued-no-max",
			params: testParameters{
				logID:               154035,
				dequeueLimit:        1,
				shouldCommit:        true,
				latestSignedRoot:    testSignedRoot16,
				dequeuedLeaves:      noLeaves,
				skipStoreSignedRoot: true,
			},
		},
		{
			desc: "nothing-queued-within-max",
			params: testParameters{
				logID:               154035,
				dequeueLimit:        1,
				shouldCommit:        true,
				latestSignedRoot:    testSignedRoot16,
				dequeuedLeaves:      noLeaves,
				skipStoreSignedRoot: true,
			},
			maxRootDuration: 15 * time.Millisecond,
		},
		{
			desc: "nothing-queued-after-max",
			params: testParameters{
				logID:            154035,
				dequeueLimit:     1,
				shouldCommit:     true,
				latestSignedRoot: testSignedRoot16,
				dequeuedLeaves:   noLeaves,
				writeRevision:    int64(testRoot16.Revision + 1),
				updatedLeaves:    &noLeaves,
				merkleNodesSet:   &noNodes,
				signer:           fixedSigner,
				storeSignedRoot:  newSignedRoot16,
			},
			maxRootDuration: 9 * time.Millisecond,
		},
		{
			desc: "nothing-queued-on-max",
			params: testParameters{
				logID:            154035,
				dequeueLimit:     1,
				shouldCommit:     true,
				latestSignedRoot: testSignedRoot16,
				dequeuedLeaves:   noLeaves,
				writeRevision:    int64(testRoot16.Revision + 1),
				updatedLeaves:    &noLeaves,
				merkleNodesSet:   &noNodes,
				signer:           fixedSigner,
				storeSignedRoot:  newSignedRoot16,
			},
			maxRootDuration: 10 * time.Millisecond,
		},
		{
			// Tests that the guard interval is being passed to storage correctly.
			// Actual operation of the window is tested by storage tests.
			desc: "guard-interval",
			params: testParameters{
				logID:               154035,
				dequeueLimit:        1,
				shouldCommit:        true,
				latestSignedRoot:    testSignedRoot16,
				dequeuedLeaves:      []*trillian.LogLeaf{},
				skipStoreSignedRoot: true,
				overrideDequeueTime: &expectedCutoffTime,
			},
			guardWindow: guardWindow,
		},
		{
			desc: "dequeue-fails",
			params: testParameters{
				logID:               154035,
				dequeueLimit:        1,
				latestSignedRoot:    testSignedRoot16,
				dequeuedError:       errors.New("dequeue"),
				skipStoreSignedRoot: true,
			},
			errStr: "dequeue",
		},
		{
			desc: "get-signed-root-fails",
			params: testParameters{
				logID:                 154035,
				dequeueLimit:          1,
				latestSignedRoot:      testSignedRoot16,
				latestSignedRootError: errors.New("root"),
				skipDequeue:           true,
				skipStoreSignedRoot:   true,
			},
			errStr: "root",
		},
		{
			desc: "update-seq-leaves-fails",
			params: testParameters{
				logID:               154035,
				writeRevision:       int64(testRoot16.Revision + 1),
				dequeueLimit:        1,
				dequeuedLeaves:      []*trillian.LogLeaf{getLeaf42()},
				latestSignedRoot:    testSignedRoot16,
				updatedLeaves:       &leaves16,
				updatedLeavesError:  errors.New("unsequenced"),
				skipStoreSignedRoot: true,
			},
			errStr: "unsequenced",
		},
		{
			desc: "set-merkle-nodes-fails",
			params: testParameters{
				logID:               154035,
				writeRevision:       int64(testRoot16.Revision + 1),
				dequeueLimit:        1,
				dequeuedLeaves:      []*trillian.LogLeaf{getLeaf42()},
				latestSignedRoot:    testSignedRoot16,
				updatedLeaves:       &leaves16,
				merkleNodesSet:      &updatedNodes,
				merkleNodesSetError: errors.New("setmerklenodes"),
				skipStoreSignedRoot: true,
			},
			errStr: "setmerklenodes",
		},
		{
			desc: "store-root-fails",
			params: testParameters{
				logID:                154035,
				writeRevision:        int64(testRoot16.Revision + 1),
				dequeueLimit:         1,
				dequeuedLeaves:       []*trillian.LogLeaf{getLeaf42()},
				latestSignedRoot:     testSignedRoot16,
				updatedLeaves:        &leaves16,
				merkleNodesSet:       &updatedNodes,
				storeSignedRoot:      nil,
				storeSignedRootError: errors.New("storesignedroot"),
				signer:               fixedSigner,
			},
			errStr: "storesignedroot",
		},
		{
			desc: "signer-fails",
			params: testParameters{
				logID:               154035,
				writeRevision:       int64(testRoot16.Revision + 1),
				dequeueLimit:        1,
				dequeuedLeaves:      []*trillian.LogLeaf{getLeaf42()},
				latestSignedRoot:    testSignedRoot16,
				updatedLeaves:       &leaves16,
				merkleNodesSet:      &updatedNodes,
				storeSignedRoot:     nil,
				signer:              signerErr,
				skipStoreSignedRoot: true,
			},
			errStr: "signerfailed",
		},
		{
			desc: "commit-fails",
			params: testParameters{
				logID:            154035,
				writeRevision:    int64(testRoot16.Revision + 1),
				dequeueLimit:     1,
				shouldCommit:     true,
				commitFails:      true,
				commitError:      errors.New("commit"),
				dequeuedLeaves:   []*trillian.LogLeaf{getLeaf42()},
				latestSignedRoot: testSignedRoot16,
				updatedLeaves:    &leaves16,
				merkleNodesSet:   &updatedNodes,
				storeSignedRoot:  nil,
				signer:           fixedSigner,
			},
			errStr: "commit",
		},
		{
			desc: "sequence-leaf-16",
			params: testParameters{
				logID:            154035,
				writeRevision:    int64(testRoot16.Revision + 1),
				dequeueLimit:     1,
				shouldCommit:     true,
				dequeuedLeaves:   []*trillian.LogLeaf{getLeaf42()},
				latestSignedRoot: testSignedRoot16,
				updatedLeaves:    &leaves16,
				merkleNodesSet:   &updatedNodes,
				storeSignedRoot:  testSignedRoot,
				signer:           fixedSigner,
			},
			wantCount: 1,
		},
		{
			desc: "prev-root-timestamp-equals",
			params: testParameters{
				logID:               154035,
				writeRevision:       int64(testRoot16.Revision + 1),
				dequeueLimit:        1,
				dequeuedLeaves:      []*trillian.LogLeaf{getLeaf42()},
				latestSignedRoot:    testSignedRoot17,
				updatedLeaves:       &leaves16,
				merkleNodesSet:      &updatedNodes,
				skipStoreSignedRoot: true,
			},
			errStr: "refusing to sign root with timestamp earlier than previous root (1464173705000000000 <= 1464173705000000000)",
		},
		{
			desc: "prev-root-timestamp-in-future",
			params: testParameters{
				logID:               154035,
				writeRevision:       int64(testRoot16.Revision + 1),
				dequeueLimit:        1,
				dequeuedLeaves:      []*trillian.LogLeaf{getLeaf42()},
				latestSignedRoot:    testSignedRoot18,
				updatedLeaves:       &leaves16,
				merkleNodesSet:      &updatedNodes,
				skipStoreSignedRoot: true,
			},
			errStr: "refusing to sign root with timestamp earlier than previous root (1464173705000000000 <= 1464173705010000000)",
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			qm := quota.NewMockManager(ctrl)
			test.params.qm = qm
			if test.wantCount > 0 {
				qm.EXPECT().PutTokens(gomock.Any(), test.wantCount, specs).Return(nil)
			}
			c, ctx := createTestContext(ctrl, test.params)
			tree := &trillian.Tree{TreeId: test.params.logID, TreeType: trillian.TreeType_LOG}

			got, err := c.sequencer.IntegrateBatch(ctx, tree, 1, test.guardWindow, test.maxRootDuration)
			if err != nil {
				if test.errStr == "" {
					t.Errorf("IntegrateBatch(%+v)=%v,%v; want _,nil", test.params, got, err)
				} else if !strings.Contains(err.Error(), test.errStr) || got != 0 {
					t.Errorf("IntegrateBatch(%+v)=%v,%v; want 0, error with %q", test.params, got, err, test.errStr)
				}
				return
			}
			if got != test.wantCount {
				t.Errorf("IntegrateBatch(%+v)=%v,nil; want %v,nil", test.params, got, test.wantCount)
			}
		})
	}
}

func TestIntegrateBatch_PutTokens(t *testing.T) {
	cryptoSigner := newSignerWithFixedSig(testSignedRoot.LogRootSignature)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	// Needed to create a signer
	hasher := rfc6962.DefaultHasher
	ts := util.NewFakeTimeSource(fakeTimeForTest)
	signer := tcrypto.NewSigner(0, cryptoSigner, crypto.SHA256)

	// Needed for IntegrateBatch calls
	const treeID int64 = 1234
	const limit = 1000
	const guardWindow = 10 * time.Second
	const maxRootDuration = 1 * time.Hour

	// Expected PutTokens specs
	specs := []quota.Spec{
		{Group: quota.Tree, Kind: quota.Read, TreeID: treeID},
		{Group: quota.Tree, Kind: quota.Write, TreeID: treeID},
		{Group: quota.Global, Kind: quota.Read},
		{Group: quota.Global, Kind: quota.Write},
	}

	oneHundredLeaves := make([]*trillian.LogLeaf, 100)
	for i := range oneHundredLeaves {
		oneHundredLeaves[i] = &trillian.LogLeaf{
			LeafValue: []byte(fmt.Sprintf("leaf-%v", i)),
		}
	}

	tests := []struct {
		desc                   string
		leaves                 []*trillian.LogLeaf
		quotaFactor            float64
		wantLeaves, wantTokens int
	}{
		{desc: "noLeaves"},
		{
			desc:       "singleLeaf",
			leaves:     []*trillian.LogLeaf{getLeaf42()},
			wantLeaves: 1,
			wantTokens: 1,
		},
		{
			desc:        "badFactor",
			leaves:      oneHundredLeaves,
			quotaFactor: 0.7, // factor <1 is normalized to 1
			wantLeaves:  100,
			wantTokens:  100,
		},
		{
			desc:        "factorOne",
			leaves:      oneHundredLeaves,
			quotaFactor: 1,
			wantLeaves:  100,
			wantTokens:  100,
		},
		{
			desc:        "10%-factor",
			leaves:      oneHundredLeaves,
			quotaFactor: 1.1,
			wantLeaves:  100,
			wantTokens:  110,
		},
	}

	any := gomock.Any()
	ctx := context.Background()
	for _, test := range tests {
		func() {
			if test.quotaFactor != 0 {
				defer func(qf float64) {
					QuotaIncreaseFactor = qf
				}(QuotaIncreaseFactor)
				QuotaIncreaseFactor = test.quotaFactor
			}

			// Correctness of operation is tested elsewhere. The focus here is the interaction
			// between Sequencer and quota.Manager.
			logTX := storage.NewMockLogTreeTX(ctrl)
			logTX.EXPECT().DequeueLeaves(any, any, any).Return(test.leaves, nil)
			logTX.EXPECT().LatestSignedLogRoot(any).Return(*testSignedRoot16, nil)
			logTX.EXPECT().WriteRevision().AnyTimes().Return(int64(testRoot16.Revision + 1))
			logTX.EXPECT().UpdateSequencedLeaves(any, any).AnyTimes().Return(nil)
			logTX.EXPECT().SetMerkleNodes(any, any).AnyTimes().Return(nil)
			logTX.EXPECT().StoreSignedLogRoot(any, any).AnyTimes().Return(nil)
			logTX.EXPECT().Commit().Return(nil)
			logTX.EXPECT().Close().Return(nil)
			logStorage := &stestonly.FakeLogStorage{TX: logTX}

			qm := quota.NewMockManager(ctrl)
			if test.wantTokens > 0 {
				qm.EXPECT().PutTokens(any, test.wantTokens, specs)
			}

			sequencer := NewSequencer(hasher, ts, logStorage, signer, nil /* mf */, qm)
			tree := &trillian.Tree{TreeId: treeID, TreeType: trillian.TreeType_LOG}
			leaves, err := sequencer.IntegrateBatch(ctx, tree, limit, guardWindow, maxRootDuration)
			if err != nil {
				t.Errorf("%v: IntegrateBatch() returned err = %v", test.desc, err)
				return
			}
			if leaves != test.wantLeaves {
				t.Errorf("%v: IntegrateBatch() returned %v leaves, want = %v", test.desc, leaves, test.wantLeaves)
			}
		}()
	}
}

func TestSignRoot(t *testing.T) {
	signerErr, err := newSignerWithErr(errors.New("signerfailed"))
	if err != nil {
		t.Fatalf("Failed to create test signer (%v)", err)
	}
	var tests = []struct {
		desc   string
		params testParameters
		errStr string
	}{
		{
			desc: "begin-tx-fails",
			params: testParameters{
				logID:               154035,
				beginFails:          true,
				skipDequeue:         true,
				skipStoreSignedRoot: true,
			},
			errStr: "TX",
		},
		{
			desc: "sign-latest-root-fails",
			params: testParameters{
				logID:                 154035,
				writeRevision:         int64(testRoot16.Revision + 1),
				dequeueLimit:          1,
				latestSignedRoot:      testSignedRoot16,
				latestSignedRootError: errors.New("root"),
				skipDequeue:           true,
				skipStoreSignedRoot:   true,
			},
			errStr: "root",
		},
		{
			desc: "signer-fails",
			params: testParameters{
				logID:               154035,
				writeRevision:       int64(testRoot16.Revision + 1),
				dequeueLimit:        1,
				latestSignedRoot:    testSignedRoot16,
				storeSignedRoot:     nil,
				signer:              signerErr,
				skipDequeue:         true,
				skipStoreSignedRoot: true,
			},
			errStr: "signer",
		},
		{
			desc: "store-root-fail",
			params: testParameters{
				logID:                154035,
				writeRevision:        int64(testRoot16.Revision + 1),
				latestSignedRoot:     testSignedRoot16,
				storeSignedRoot:      nil,
				storeSignedRootError: errors.New("storesignedroot"),
				signer:               fixedSigner,
				skipDequeue:          true,
			},
			errStr: "storesignedroot",
		},
		{
			desc: "root-commit-fail",
			params: testParameters{
				logID:            154035,
				writeRevision:    int64(testRoot16.Revision + 1),
				shouldCommit:     true,
				commitFails:      true,
				commitError:      errors.New("commit"),
				latestSignedRoot: testSignedRoot16,
				storeSignedRoot:  nil,
				signer:           fixedSigner,
				skipDequeue:      true,
			},
			errStr: "commit",
		},
		{
			desc: "existing-root",
			params: testParameters{
				logID:            154035,
				writeRevision:    int64(testRoot16.Revision + 1),
				latestSignedRoot: testSignedRoot16,
				storeSignedRoot:  newSignedRoot16,
				signer:           fixedSigner,
				shouldCommit:     true,
				skipDequeue:      true,
			},
		},
		{
			desc: "no-existing-root",
			params: testParameters{
				logID:                 154035,
				writeRevision:         int64(testRoot16.Revision + 1),
				latestSignedRoot:      &trillian.SignedLogRoot{},
				latestSignedRootError: storage.ErrTreeNeedsInit,
				skipStoreSignedRoot:   true,
				signer:                fixedSigner,
				shouldCommit:          false,
				skipDequeue:           true,
			},
			errStr: storage.ErrTreeNeedsInit.Error(),
		},
	}

	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			c, ctx := createTestContext(ctrl, test.params)
			tree := &trillian.Tree{TreeId: test.params.logID, TreeType: trillian.TreeType_LOG}
			err := c.sequencer.SignRoot(ctx, tree)
			if test.errStr != "" {
				if err == nil {
					t.Errorf("SignRoot(%+v)=nil; want error with %q", test.params, test.errStr)
				} else if !strings.Contains(err.Error(), test.errStr) {
					t.Errorf("SignRoot(%+v)=%v; want error with %q", test.params, err, test.errStr)
				}
				return
			}
			if err != nil {
				t.Errorf("SignRoot()=%v; want nil", err)
			}
		})
	}
}
