// SPDX-License-Identifier: ice License 1.0

package coindistribution

import (
	"context"
	"errors"
	"math/rand"
	"os"
	"testing"
	stdlibtime "time"

	"github.com/stretchr/testify/require"

	"github.com/ice-blockchain/wintr/connectors/storage/v2"
	"github.com/ice-blockchain/wintr/time"
)

func maybeSkipTest(t *testing.T) {
	t.Helper()

	run := os.Getenv("TEST_RUN_WITH_DATABASE")
	if run != "yes" {
		t.Skip("TEST_RUN_WITH_DATABASE is not set to 'yes'")
	}
}

func RandStringBytes(n int) string {
	const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n) //nolint:makezero //.
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))] //nolint:gosec //.
	}

	return string(b)
}

func helperAddNewPendingTransaction(ctx context.Context, t *testing.T, proc *coinProcessor, count int) {
	t.Helper()

	const stmt = `
INSERT INTO pending_coin_distributions
	(created_at, day, internal_id, iceflakes, user_id, eth_address)
VALUES (now(), CURRENT_DATE, 1, $1, $2, $3)`

	t.Logf("adding %v new pending transaction(s)", count)
	for i := 0; i < count; i++ {
		_, err := storage.Exec(ctx, proc.DB, stmt, rand.Int63n(1_000_000)+1, RandStringBytes(10), RandStringBytes(16)) //nolint:gosec //.
		require.NoError(t, err)
	}
}

func helperTruncatePendingTransactions(ctx context.Context, t *testing.T, db *storage.DB) {
	t.Helper()

	const stmt = `TRUNCATE TABLE pending_coin_distributions`

	_, err := storage.Exec(ctx, db, stmt)
	require.NoError(t, err)

	_, err = storage.Exec(ctx, db, `UPDATE global SET value = 'true' WHERE key = 'coin_distributer_enabled'`)
	require.NoError(t, err)
}

func TestBatchPrepareFetch(t *testing.T) { //nolint:paralleltest //.
	maybeSkipTest(t)
	ctx := context.TODO()
	proc := newCoinProcessor(nil, storage.MustConnect(ctx, ddl, applicationYamlKey), &config{})
	require.NotNil(t, proc)
	defer proc.Close()

	helperTruncatePendingTransactions(ctx, t, proc.DB)

	t.Run("NotEnoughData", func(t *testing.T) { //nolint:paralleltest //.
		_, err := proc.BatchPrepareFetch(ctx)
		require.ErrorIs(t, err, errNotEnoughData)
	})
	t.Run("Fetch", func(t *testing.T) { //nolint:paralleltest //.
		helperAddNewPendingTransaction(ctx, t, proc, 100)
		b, err := proc.BatchPrepareFetch(ctx)
		require.NoError(t, err)
		require.Len(t, b.Records, 100)
		for _, r := range b.Records {
			require.Equal(t, ethApiStatusPending, r.EthStatus)
		}
	})
}

func TestGetGasPrice(t *testing.T) { //nolint:tparallel //.
	t.Parallel()

	ctx := context.TODO()
	proc := newCoinProcessor(new(mockedDummyEthClient), nil, &config{})
	require.NotNil(t, proc)
	defer proc.Close()

	gas, err := proc.GetGasPrice(ctx)
	require.NoError(t, err)
	require.NotNil(t, gas)

	t.Logf("gas initial: %v", gas)

	t.Run("FromCache", func(t *testing.T) {
		gasNew, cacheErr := proc.GetGasPrice(ctx)
		require.NoError(t, cacheErr)
		require.NotNil(t, gasNew)
		require.Equal(t, gas, gasNew)
	})

	proc.gasPriceCache.time = time.New(stdlibtime.Now().Add(-gasPriceCacheTTL - stdlibtime.Second))

	gasNew, err := proc.GetGasPrice(ctx)
	require.NoError(t, err)
	require.NotNil(t, gasNew)

	t.Logf("gas updated: %v", gasNew)

	require.NotEqual(t, gas, gasNew)
}

func TestProcessorDistributeAccepted(t *testing.T) { //nolint:paralleltest //.
	maybeSkipTest(t)
	ctx := context.TODO()
	proc := newCoinProcessor(new(mockedDummyEthClient), storage.MustConnect(ctx, ddl, applicationYamlKey), &config{})
	require.NotNil(t, proc)
	defer proc.Close()

	helperTruncatePendingTransactions(ctx, t, proc.DB)
	helperAddNewPendingTransaction(ctx, t, proc, batchSize*3)

	ch := make(chan *batch, 3)
	proc.Start(ctx, ch)
	for i := 0; i < 3; i++ {
		data := <-ch
		t.Logf("batch: %v: processed with %v record(s)", data.ID, len(data.Records))
		for _, r := range data.Records {
			require.Equal(t, ethApiStatusAccepted, r.EthStatus)
		}
	}
}

func TestProcessorDistributeRejected(t *testing.T) { //nolint:paralleltest //.
	maybeSkipTest(t)
	ctx := context.TODO()
	proc := newCoinProcessor(&mockedDummyEthClient{dropErr: errors.New("drop error")}, //nolint:goerr113 //.
		storage.MustConnect(ctx, ddl, applicationYamlKey),
		&config{},
	)
	require.NotNil(t, proc)
	defer proc.Close()

	helperTruncatePendingTransactions(ctx, t, proc.DB)
	helperAddNewPendingTransaction(ctx, t, proc, 30)

	ch := make(chan *batch, 3)
	proc.Start(ctx, ch)

	data := <-ch
	t.Logf("batch: %v: processed with %v record(s)", data.ID, len(data.Records))
	for _, r := range data.Records {
		require.Equal(t, ethApiStatusRejected, r.EthStatus)
	}

	select {
	case <-ch:
		t.Fatal("unexpected batch")
	default:
	}
}

func TestIsInTimeWindow(t *testing.T) {
	t.Parallel()

	require.True(t, isInTimeWindow(10, 10, 22))
	require.True(t, isInTimeWindow(23, 22, 6))
	require.True(t, isInTimeWindow(6, 22, 6))
	require.True(t, isInTimeWindow(0, 22, 6))
	require.False(t, isInTimeWindow(17, 22, 6))
	require.False(t, isInTimeWindow(23, 10, 22))
	require.False(t, isInTimeWindow(9, 10, 22))
	require.True(t, isInTimeWindow(2, 0, 23))
	require.True(t, isInTimeWindow(0, 0, 0))
	require.True(t, isInTimeWindow(1, 0, 0))

	require.Panics(t, func() {
		isInTimeWindow(24, 0, 0)
	})
	require.Panics(t, func() {
		isInTimeWindow(0, -1, 0)
	})
	require.Panics(t, func() {
		isInTimeWindow(0, 0, 24)
	})
}

func TestProcessorTriggerOnDemand(t *testing.T) { //nolint:paralleltest //.
	maybeSkipTest(t)
	ctx := context.TODO()
	now := time.Now()
	proc := newCoinProcessor(&mockedDummyEthClient{},
		storage.MustConnect(ctx, ddl, applicationYamlKey),
		&config{
			StartHours: now.Hour() - 2,
			EndHours:   now.Hour() - 1,
		},
	)
	require.NotNil(t, proc)
	defer proc.Close()

	helperTruncatePendingTransactions(ctx, t, proc.DB)
	helperAddNewPendingTransaction(ctx, t, proc, batchSize*4)

	ch := make(chan *batch, 4)
	proc.Start(ctx, ch)
	require.True(t, proc.isBlocked())

	select {
	case <-ch:
		t.Fatal("unexpected batch outside of time window")

	case <-stdlibtime.After(stdlibtime.Second * 10):
		t.Log("firing trigger")
		require.NoError(t, databaseSetValue(ctx, proc.DB, configKeyCoinDistributerOnDemand, true))
	}

	for i := 0; i < 4; i++ {
		data := <-ch
		t.Logf("batch: %v: processed with %v record(s)", data.ID, len(data.Records))
		for _, r := range data.Records {
			require.Equal(t, ethApiStatusAccepted, r.EthStatus)
		}
	}

	require.False(t, proc.IsOnDemandMode(ctx))
}
