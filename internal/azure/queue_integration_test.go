//go:build integration

// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue/v2"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// T029: validates storageQueueSource (queue.go) against a real Azurite
// container -- the real azqueue wire protocol, not a fake. Azurite is
// Microsoft's official Azure Storage emulator and speaks the same REST API
// real Azure Storage Queues do, so a QueueSource that works here is
// confirmed to work against the real service too (research.md R10).
//
// Azurite's well-known development account/key (documented by Microsoft,
// not a secret): devstoreaccount1 /
// Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==
const (
	azuriteAccountName = "devstoreaccount1"
	azuriteAccountKey  = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	testQueueName      = "kv-change-notifications"
)

// startAzurite launches an Azurite container (queue service only isn't an
// option upstream -- Azurite always serves blob/queue/table together -- so
// this only waits on/exposes the queue port, 10001) and returns a
// storageQueueSource wired to a freshly created test queue inside it, plus
// a raw *azqueue.QueueClient for setup/assertions the QueueSource interface
// doesn't expose (e.g. seeding messages with a controlled DequeueCount).
func startAzurite(t *testing.T) (*storageQueueSource, *azqueue.QueueClient) {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "mcr.microsoft.com/azure-storage/azurite:3.36.0",
		ExposedPorts: []string{"10001/tcp"},
		Cmd:          []string{"azurite-queue", "--queueHost", "0.0.0.0", "--skipApiVersionCheck"},
		WaitingFor:   wait.ForListeningPort("10001/tcp").WithStartupTimeout(60 * time.Second),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start azurite container: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate azurite container: %v", err)
		}
	})

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("azurite container host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "10001/tcp")
	if err != nil {
		t.Fatalf("azurite container mapped port: %v", err)
	}

	serviceURL := fmt.Sprintf("http://%s:%s/%s", host, port.Port(), azuriteAccountName)

	cred, err := azqueue.NewSharedKeyCredential(azuriteAccountName, azuriteAccountKey)
	if err != nil {
		t.Fatalf("build azurite shared key credential: %v", err)
	}
	svc, err := azqueue.NewServiceClientWithSharedKeyCredential(serviceURL, cred, nil)
	if err != nil {
		t.Fatalf("build azurite service client: %v", err)
	}

	qc := svc.NewQueueClient(testQueueName)
	if _, err := qc.Create(ctx, nil); err != nil {
		t.Fatalf("create test queue: %v", err)
	}

	return &storageQueueSource{client: qc}, qc
}

// TestStorageQueueSource_BatchReceiveAndDelete exercises a batch receive of
// 32 messages followed by deleting each (contracts/queue-message.md
// processing rule 5), against the real wire protocol.
func TestStorageQueueSource_BatchReceiveAndDelete(t *testing.T) {
	source, qc := startAzurite(t)
	ctx := context.Background()

	const batchSize = 32
	for i := range batchSize {
		if _, err := qc.EnqueueMessage(ctx, fmt.Sprintf("payload-%d", i), nil); err != nil {
			t.Fatalf("enqueue message %d: %v", i, err)
		}
	}

	msgs, err := source.Receive(ctx, batchSize)
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if len(msgs) != batchSize {
		t.Fatalf("Receive() returned %d messages, want %d", len(msgs), batchSize)
	}

	for _, m := range msgs {
		if m.ID == "" || m.PopReceipt == "" {
			t.Fatalf("message missing ID/PopReceipt: %+v", m)
		}
		if err := source.Delete(ctx, m.ID, m.PopReceipt); err != nil {
			t.Fatalf("Delete(%s) error = %v", m.ID, err)
		}
	}

	// Queue should now be empty: a further receive returns nothing.
	remaining, err := source.Receive(ctx, batchSize)
	if err != nil {
		t.Fatalf("Receive() after drain error = %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected an empty queue after deleting all messages, got %d left", len(remaining))
	}
}

// TestStorageQueueSource_DequeueCountAndVisibilityTimeout confirms
// DequeueCount increments across redeliveries once a message's visibility
// timeout expires without it being deleted -- the signal the poison
// threshold (contracts/queue-message.md rule 6, DequeueCount > 5) depends
// on. Uses a 1s visibility timeout so the test does not need to sleep long.
func TestStorageQueueSource_DequeueCountAndVisibilityTimeout(t *testing.T) {
	source, qc := startAzurite(t)
	ctx := context.Background()

	if _, err := qc.EnqueueMessage(ctx, "poison-candidate", nil); err != nil {
		t.Fatalf("enqueue message: %v", err)
	}

	first, err := source.Receive(ctx, 1)
	if err != nil {
		t.Fatalf("first Receive() error = %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("expected 1 message, got %d", len(first))
	}
	if first[0].DequeueCount != 1 {
		t.Fatalf("first receive DequeueCount = %d, want 1", first[0].DequeueCount)
	}

	// Do NOT delete: let the default (30s, since Receive never overrides
	// it) visibility timeout expire so the message becomes receivable
	// again with an incremented DequeueCount, exactly like a
	// crash/redeliver in production.
	deadline := time.Now().Add(45 * time.Second)
	var second []QueueMessage
	for time.Now().Before(deadline) {
		second, err = source.Receive(ctx, 1)
		if err != nil {
			t.Fatalf("second Receive() error = %v", err)
		}
		if len(second) == 1 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(second) != 1 {
		t.Fatalf("message did not become visible again after its visibility timeout elapsed")
	}
	if second[0].ID != first[0].ID {
		t.Fatalf("redelivered message ID = %q, want %q", second[0].ID, first[0].ID)
	}
	if second[0].DequeueCount <= first[0].DequeueCount {
		t.Fatalf("DequeueCount did not increase on redelivery: first=%d second=%d",
			first[0].DequeueCount, second[0].DequeueCount)
	}

	if err := source.Delete(ctx, second[0].ID, second[0].PopReceipt); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

// TestStorageQueueSource_EventGridPayloadRoundtrip roundtrips the exact
// Base64-encoded Event Grid example payload from
// specs/001-akv-eventgrid-sync/contracts/queue-message.md through a real
// Azurite queue: this is what T029 means by "Base64-encoded Event Grid
// payload roundtrip" -- QueueSource itself never decodes Text (that's
// internal/events.Parse's job), it only has to hand back byte-for-byte what
// was enqueued.
func TestStorageQueueSource_EventGridPayloadRoundtrip(t *testing.T) {
	source, qc := startAzurite(t)
	ctx := context.Background()

	const eventGridJSON = `{
  "id": "6a6cbc37-2222-4444-8888-63c2fc7b0001",
  "topic": "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.KeyVault/vaults/my-vault",
  "subject": "my-app-password",
  "eventType": "Microsoft.KeyVault.SecretNewVersionCreated",
  "eventTime": "2026-08-21T10:12:33.1234567Z",
  "dataVersion": "1",
  "metadataVersion": "1",
  "data": {
    "Id": "https://my-vault.vault.azure.net/secrets/my-app-password/abc123",
    "VaultName": "my-vault",
    "ObjectType": "Secret",
    "ObjectName": "my-app-password",
    "Version": "abc123",
    "NBF": null,
    "EXP": null
  }
}`
	encoded := base64.StdEncoding.EncodeToString([]byte(eventGridJSON))

	if _, err := qc.EnqueueMessage(ctx, encoded, nil); err != nil {
		t.Fatalf("enqueue event grid payload: %v", err)
	}

	msgs, err := source.Receive(ctx, 1)
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Text != encoded {
		t.Fatalf("roundtripped message text does not match what was enqueued:\ngot:  %q\nwant: %q",
			msgs[0].Text, encoded)
	}

	decoded, err := base64.StdEncoding.DecodeString(msgs[0].Text)
	if err != nil {
		t.Fatalf("decode roundtripped message: %v", err)
	}
	if string(decoded) != eventGridJSON {
		t.Fatalf("decoded roundtripped message does not match the original event grid JSON")
	}

	if err := source.Delete(ctx, msgs[0].ID, msgs[0].PopReceipt); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}
