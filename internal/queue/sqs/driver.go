package sqs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/agentculture/culture-nodes/internal/awsauth"
	"github.com/agentculture/culture-nodes/internal/queue"
)

// schemaVersion is the message body schema version this build of the driver
// produces and understands. See the package doc's "Message body: a
// versioned, opaque reference only" section for why Receive skips, rather
// than errors on, any other value.
const schemaVersion = 1

// dedupAttributeName is the *message attribute* (not the FIFO-only
// MessageDeduplicationId request parameter) Publish attaches to every
// message -- informational only, never a deduplication mechanism. See the
// package doc's "The MessageDeduplicationId attribute is informational
// only" section.
const dedupAttributeName = "MessageDeduplicationId"

// sqsMaxWaitTimeSeconds and sqsMaxReceiveBatch are hard limits SQS itself
// imposes on ReceiveMessage, not choices this driver makes.
const (
	sqsMaxWaitTimeSeconds = 20 * time.Second
	sqsMaxReceiveBatch    = 10
)

// defaultVisibilityTimeout is used when Config.VisibilityTimeout is left
// zero. It should normally be tuned to the caller's lease duration (task
// t7's ClaimWork leaseDuration): a delivery a worker is still processing
// should stay invisible to other receivers for at least as long as the
// worker's Postgres lease is valid, so a slow-but-alive worker does not
// race a second delivery of the same signal for no reason (harmless if it
// happens -- see the package doc -- but wasteful).
const defaultVisibilityTimeout = 30 * time.Second

// messageBody is the schema-versioned, compact JSON body Publish writes and
// Receive parses. It carries exactly queue.WorkRef's three fields, nothing
// else -- see internal/queue's "signals are references, never payloads"
// rule.
type messageBody struct {
	V           int    `json:"v"`
	WorkID      string `json:"work_id"`
	NodeRunID   string `json:"node_run_id,omitempty"`
	NamespaceID string `json:"namespace_id"`
}

// Config configures a Driver.
type Config struct {
	// QueueURL is the target SQS queue's URL. Required.
	QueueURL string
	// Region is the AWS region the queue lives in. Empty defaults to
	// "us-east-1"; against real SQS that default is almost certainly
	// wrong for your queue and should be set explicitly. A placeholder
	// value is fine when Endpoint points at a fake or LocalStack, since
	// neither inspects it meaningfully.
	Region string
	// Endpoint overrides the SDK's default AWS endpoint resolution --
	// point it at a LocalStack container or (in this package's own tests)
	// the in-process fake SQS server. Empty means "talk to real AWS SQS".
	Endpoint string
	// MaxWait bounds the WaitTimeSeconds this driver ever sends on a single
	// ReceiveMessage call, and therefore how long a Receive call with a
	// large caller-supplied wait can go between polls. SQS itself caps
	// WaitTimeSeconds at 20s regardless of this value. Zero means "use
	// SQS's own 20s maximum".
	MaxWait time.Duration
	// VisibilityTimeout is the visibility timeout Receive requests for
	// every delivery -- how long SQS withholds a received message from
	// other receivers before it becomes redeliverable again. Zero means
	// defaultVisibilityTimeout.
	VisibilityTimeout time.Duration
	// Credentials, when non-nil, overrides the SDK's default credential
	// chain for New. Production deployments normally leave this nil and
	// either accept the SDK's own default chain or use NewDriverFromAuth
	// instead of New, which resolves credentials through
	// internal/awsauth.LoadConfig (task t17's shared IRSA-ready resolver);
	// this package's own tests set it to a static, meaningless credential
	// pair pointed at the fake server, which never validates a signature.
	Credentials aws.CredentialsProvider
	// HTTPClient, when non-nil, overrides the SDK's default HTTP client.
	// This package's own tests use it to point the SDK at an
	// httptest.Server without touching any process-wide transport state.
	HTTPClient *http.Client
	// MaxAttempts overrides the SDK's default retry attempt count for
	// every request this Driver issues. Zero means "use the SDK default".
	// This package's own drop-chaos test sets it to 1 (no retries) so a
	// forced SendMessage failure is observed exactly once per Publish call
	// rather than retried transparently out from under the test.
	MaxAttempts int
	// Logf receives a formatted diagnostic whenever Receive skips a
	// message it cannot use (an unrecognized schema version, or a body
	// that fails to parse as JSON at all) -- see the package doc's
	// "Message body" section. Nil means "log.Printf", never "silently
	// drop the diagnostic".
	Logf func(format string, args ...any)
}

// Driver is a queue.Queue backed by an SQS Standard queue. It is safe for
// concurrent use: sqs.Client is, and Driver carries no other mutable state.
type Driver struct {
	client            *sqs.Client
	queueURL          string
	maxWait           time.Duration
	visibilityTimeout time.Duration
	logf              func(format string, args ...any)
}

var _ queue.Queue = (*Driver)(nil)

// New builds a Driver from cfg, resolving AWS credentials and region
// through the standard SDK config chain (overridden by cfg.Credentials when
// set) and pointing the client at cfg.Endpoint when non-empty.
//
// See NewDriverFromAuth for an alternative constructor that resolves
// credentials through internal/awsauth.LoadConfig (task t17's shared
// IRSA-ready resolver) instead of this function's own inline
// awsconfig.LoadDefaultConfig option list. New is unchanged and remains the
// default path -- NewDriverFromAuth is additive.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	if cfg.QueueURL == "" {
		return nil, fmt.Errorf("queue/sqs: New: QueueURL is required")
	}

	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if cfg.Credentials != nil {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(cfg.Credentials))
	}
	if cfg.HTTPClient != nil {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(cfg.HTTPClient))
	}
	if cfg.MaxAttempts > 0 {
		loadOpts = append(loadOpts, awsconfig.WithRetryMaxAttempts(cfg.MaxAttempts))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("queue/sqs: New: load AWS config: %w", err)
	}

	return newDriver(awsCfg, cfg)
}

// NewDriverFromAuth builds a Driver the same way New does, except AWS
// credentials and region are resolved through internal/awsauth.LoadConfig
// (task t17) rather than this package's own awsconfig.LoadDefaultConfig
// option list -- giving this driver IRSA support and Source reporting (via
// authOpts.Logf) for free.
//
// cfg.Credentials is ignored on this path: authOpts is the single source of
// authentication configuration. cfg.Region, if authOpts.Region is empty, is
// used as authOpts.Region's fallback (and "us-east-1" if both are empty,
// matching New's own default) -- everything else on cfg (Endpoint,
// HTTPClient, MaxAttempts, MaxWait, VisibilityTimeout, Logf) applies exactly
// as it does for New.
func NewDriverFromAuth(ctx context.Context, authOpts awsauth.Options, cfg Config) (*Driver, error) {
	if cfg.QueueURL == "" {
		return nil, fmt.Errorf("queue/sqs: NewDriverFromAuth: QueueURL is required")
	}

	if authOpts.Region == "" {
		authOpts.Region = cfg.Region
	}
	if authOpts.Region == "" {
		authOpts.Region = "us-east-1"
	}

	awsCfg, _, err := awsauth.LoadConfig(ctx, authOpts)
	if err != nil {
		return nil, fmt.Errorf("queue/sqs: NewDriverFromAuth: resolve AWS credentials: %w", err)
	}

	if cfg.HTTPClient != nil {
		awsCfg.HTTPClient = cfg.HTTPClient
	}
	if cfg.MaxAttempts > 0 {
		awsCfg.RetryMaxAttempts = cfg.MaxAttempts
	}

	return newDriver(awsCfg, cfg)
}

// newDriver builds a Driver from an already-resolved aws.Config, shared by
// New and NewDriverFromAuth.
func newDriver(awsCfg aws.Config, cfg Config) (*Driver, error) {
	client := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) {
		if cfg.Endpoint != "" {
			endpoint := cfg.Endpoint
			o.BaseEndpoint = &endpoint
		}
	})

	maxWait := cfg.MaxWait
	if maxWait <= 0 || maxWait > sqsMaxWaitTimeSeconds {
		maxWait = sqsMaxWaitTimeSeconds
	}

	visibilityTimeout := cfg.VisibilityTimeout
	if visibilityTimeout <= 0 {
		visibilityTimeout = defaultVisibilityTimeout
	}

	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}

	return &Driver{
		client:            client,
		queueURL:          cfg.QueueURL,
		maxWait:           maxWait,
		visibilityTimeout: visibilityTimeout,
		logf:              logf,
	}, nil
}

// Publish sends ref as a schema-versioned JSON body via SQS SendMessage.
// See the package doc for why the message body carries only WorkRef's three
// fields and why the dedup attribute is informational.
//
// Publish makes no idempotency promise of its own -- unlike the Postgres
// driver's Publish, SQS Standard has no server-side concept of "insert this
// WorkID at most once". That is fine because Publish's only caller
// (internal/events.Relay) already reuses a stable WorkID across a
// crash-retry, and every downstream consumer of a delivery is required to
// treat re-delivery as harmless (see the package doc).
func (d *Driver) Publish(ctx context.Context, ref queue.WorkRef) error {
	switch {
	case ref.WorkID == "":
		return fmt.Errorf("queue/sqs: Publish: WorkID is required")
	case ref.NamespaceID == "":
		return fmt.Errorf("queue/sqs: Publish: NamespaceID is required")
	}

	body, err := json.Marshal(messageBody{
		V:           schemaVersion,
		WorkID:      ref.WorkID,
		NodeRunID:   ref.NodeRunID,
		NamespaceID: ref.NamespaceID,
	})
	if err != nil {
		return fmt.Errorf("queue/sqs: Publish: encode body: %w", err)
	}

	_, err = d.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    &d.queueURL,
		MessageBody: aws.String(string(body)),
		MessageAttributes: map[string]types.MessageAttributeValue{
			dedupAttributeName: {
				DataType:    aws.String("String"),
				StringValue: aws.String(ref.WorkID),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("queue/sqs: Publish: %w", err)
	}
	return nil
}

// Receive returns up to max ready deliveries, long-polling SQS itself for
// up to wait (bounded by Config.MaxWait and SQS's own 20s hard limit). A
// message whose body does not parse as JSON, or whose schema version this
// driver does not recognize, is skipped -- logged via Config.Logf, never
// returned, never causing an error -- and deliberately left un-acked (see
// the package doc) for SQS's own redrive/DLQ handling.
func (d *Driver) Receive(ctx context.Context, max int, wait time.Duration) ([]queue.Delivery, error) {
	if max <= 0 {
		max = 1
	}
	if max > sqsMaxReceiveBatch {
		max = sqsMaxReceiveBatch
	}
	if wait <= 0 {
		return d.receiveOnce(ctx, max, 0)
	}

	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)

		perCall := remaining
		if perCall > d.maxWait {
			perCall = d.maxWait
		}
		if perCall > sqsMaxWaitTimeSeconds {
			perCall = sqsMaxWaitTimeSeconds
		}
		if perCall < time.Second {
			// WaitTimeSeconds is whole seconds; round a sub-second budget
			// up to one full second rather than skipping the call
			// (returning empty) or truncating to a zero-wait short poll
			// that would busy-loop for the remainder. This means a
			// caller's wait can overshoot by up to ~1s -- an accepted,
			// documented cost of SQS's whole-second WaitTimeSeconds
			// granularity, real or faked.
			perCall = time.Second
		}

		deliveries, err := d.receiveOnce(ctx, max, perCall)
		if err != nil {
			return nil, err
		}
		if len(deliveries) > 0 {
			return deliveries, nil
		}
		if time.Until(deadline) <= 0 {
			return nil, nil
		}
		// receiveOnce already blocked inside SQS's own long poll for
		// perCall, so looping back around is not a busy loop.
	}
}

func (d *Driver) receiveOnce(ctx context.Context, max int, wait time.Duration) ([]queue.Delivery, error) {
	waitSeconds := int32(wait / time.Second)

	out, err := d.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:              &d.queueURL,
		MaxNumberOfMessages:   int32(max),
		WaitTimeSeconds:       waitSeconds,
		VisibilityTimeout:     int32(d.visibilityTimeout / time.Second),
		MessageAttributeNames: []string{"All"},
	})
	if err != nil {
		return nil, fmt.Errorf("queue/sqs: Receive: %w", err)
	}

	deliveries := make([]queue.Delivery, 0, len(out.Messages))
	for _, m := range out.Messages {
		ref, ok := d.parseWorkRef(m)
		if !ok {
			continue
		}
		deliveries = append(deliveries, queue.Delivery{WorkRef: ref, Receipt: aws.ToString(m.ReceiptHandle)})
	}
	return deliveries, nil
}

// parseWorkRef decodes m's body into a queue.WorkRef, reporting ok=false
// (after logging a diagnostic) for a body that is not valid JSON or whose
// schema version this driver does not recognize.
func (d *Driver) parseWorkRef(m types.Message) (queue.WorkRef, bool) {
	id := aws.ToString(m.MessageId)
	body := aws.ToString(m.Body)

	var parsed messageBody
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		d.logf("queue/sqs: Receive: skipping message %s: body is not valid JSON: %v", id, err)
		return queue.WorkRef{}, false
	}
	if parsed.V != schemaVersion {
		d.logf("queue/sqs: Receive: skipping message %s: unsupported schema version %d (this driver understands %d)", id, parsed.V, schemaVersion)
		return queue.WorkRef{}, false
	}
	return queue.WorkRef{
		WorkID:      parsed.WorkID,
		NodeRunID:   parsed.NodeRunID,
		NamespaceID: parsed.NamespaceID,
	}, true
}

// Ack deletes d's message via DeleteMessage. Acking a delivery whose
// receipt SQS no longer recognizes (already deleted, or its visibility
// timeout already expired and it was redelivered under a new receipt
// handle) is treated as a harmless no-op, matching the queue.Queue
// contract's idempotence requirement.
func (d *Driver) Ack(ctx context.Context, del queue.Delivery) error {
	if del.Receipt == "" {
		return fmt.Errorf("queue/sqs: Ack: Receipt is required")
	}

	_, err := d.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      &d.queueURL,
		ReceiptHandle: &del.Receipt,
	})
	if err != nil {
		if isStaleReceipt(err) {
			return nil
		}
		return fmt.Errorf("queue/sqs: Ack: %w", err)
	}
	return nil
}

// Delay changes d's visibility timeout to delay (via ChangeMessageVisibility
// -- a negative delay is treated as zero, matching queue.Queue's contract).
// Like Ack, delaying a delivery whose receipt is no longer valid is a
// harmless no-op rather than an error.
func (d *Driver) Delay(ctx context.Context, del queue.Delivery, delay time.Duration) error {
	if del.Receipt == "" {
		return fmt.Errorf("queue/sqs: Delay: Receipt is required")
	}
	if delay < 0 {
		delay = 0
	}

	_, err := d.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          &d.queueURL,
		ReceiptHandle:     &del.Receipt,
		VisibilityTimeout: int32(delay / time.Second),
	})
	if err != nil {
		if isStaleReceipt(err) {
			return nil
		}
		return fmt.Errorf("queue/sqs: Delay: %w", err)
	}
	return nil
}

// isStaleReceipt reports whether err is one of the two typed SQS errors
// that mean "this receipt handle no longer refers to anything I can act
// on" (an invalid/expired handle, or a message that is no longer
// in-flight) -- the SQS-specific shape of the "already acked/delayed, or
// never existed" case queue.Queue's Ack and Delay contracts require callers
// to treat as a no-op, not an error.
func isStaleReceipt(err error) bool {
	var invalid *types.ReceiptHandleIsInvalid
	if errors.As(err, &invalid) {
		return true
	}
	var notInflight *types.MessageNotInflight
	return errors.As(err, &notInflight)
}
