// Copyright kvsynk8s contributors.
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue/v2"
)

// QueueMessage is one batch-received message from the Key Vault change
// notification queue (contracts/queue-message.md). Text is the raw message
// body exactly as the queue stores it -- still Base64-encoded per the
// contract's envelope rules; internal/events.Parse is what decodes and
// interprets it. This package never inspects Text's content and never logs
// it: not because a value could plausibly appear here (the queue carries no
// secret values by design), but because constitution I's redaction rule is
// unconditional.
type QueueMessage struct {
	// ID identifies the message for a later Delete call and for
	// logging/correlation. Never a secret value.
	ID string
	// PopReceipt authorizes deleting or updating this specific delivery of
	// the message; required alongside ID by Delete.
	PopReceipt string
	// Text is the raw (still Base64-encoded) message body.
	Text string
	// DequeueCount is how many times this message has been delivered.
	// DequeueCount > 5 is this operator's poison-message threshold
	// (contracts/queue-message.md rule 6).
	DequeueCount int64
}

// QueueSource pulls Event Grid Key Vault change notifications from the Azure
// Storage Queue Event Grid is configured to deliver them to (research R3,
// R6). Implementations MUST NOT log or otherwise surface QueueMessage.Text
// content beyond returning it as-is (constitution I).
type QueueSource interface {
	// Receive batch-receives up to maxMessages currently visible messages
	// (contracts/queue-message.md: batch size 32). A nil/empty slice with a
	// nil error means the queue is currently empty.
	Receive(ctx context.Context, maxMessages int32) ([]QueueMessage, error)

	// Delete removes a message this caller has finished with: either its
	// matching syncs were accepted for processing (rule 5), or it was
	// discarded (malformed, unmatched, or poison -- rules 1, 2/3, 6).
	Delete(ctx context.Context, messageID, popReceipt string) error
}

// queueClient is the subset of *azqueue.QueueClient that storageQueueSource
// needs, kept as an interface so construction (NewQueueSource) stays the only
// place that talks to the real SDK client type.
type queueClient interface {
	DequeueMessages(ctx context.Context, o *azqueue.DequeueMessagesOptions) (azqueue.DequeueMessagesResponse, error)
	DeleteMessage(ctx context.Context, messageID string, popReceipt string, o *azqueue.DeleteMessageOptions) (azqueue.DeleteMessageResponse, error)
}

// storageQueueSource is the azqueue-backed QueueSource.
type storageQueueSource struct {
	client queueClient
}

// NewQueueSource builds a QueueSource for the Storage Queue at queueURL,
// authenticating with azidentity.NewDefaultAzureCredential (workload identity
// in-cluster; falls back through the standard credential chain locally, per
// constitution V: short-lived, platform-issued credentials only).
func NewQueueSource(queueURL string) (QueueSource, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create default azure credential: %w", err)
	}
	client, err := azqueue.NewQueueClient(queueURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("create storage queue client: %w", err)
	}
	return &storageQueueSource{client: client}, nil
}

// Receive implements QueueSource.
func (s *storageQueueSource) Receive(ctx context.Context, maxMessages int32) ([]QueueMessage, error) {
	resp, err := s.client.DequeueMessages(ctx, &azqueue.DequeueMessagesOptions{NumberOfMessages: &maxMessages})
	if err != nil {
		return nil, fmt.Errorf("dequeue messages: %w", err)
	}

	out := make([]QueueMessage, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		if m == nil {
			continue
		}
		out = append(out, QueueMessage{
			ID:           stringVal(m.MessageID),
			PopReceipt:   stringVal(m.PopReceipt),
			Text:         stringVal(m.MessageText),
			DequeueCount: int64Val(m.DequeueCount),
		})
	}
	return out, nil
}

// Delete implements QueueSource.
func (s *storageQueueSource) Delete(ctx context.Context, messageID, popReceipt string) error {
	if _, err := s.client.DeleteMessage(ctx, messageID, popReceipt, nil); err != nil {
		return fmt.Errorf("delete message %s: %w", messageID, err)
	}
	return nil
}

func stringVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func int64Val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
