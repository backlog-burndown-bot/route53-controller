package record_set

import (
	"context"
	"errors"
	"testing"

	svcapitypes "github.com/aws-controllers-k8s/route53-controller/apis/v1alpha1"
	ackrequeue "github.com/aws-controllers-k8s/runtime/pkg/requeue"
	"github.com/aws/aws-sdk-go-v2/aws"
	smithy "github.com/aws/smithy-go"
)

func Test_getDNSName(t *testing.T) {
	rm := &resourceManager{}

	tests := []struct {
		testName   string
		recordName string
		domain     string
		want       string
	}{
		{
			testName:   "nil name returns hosted zone domain",
			recordName: "",
			domain:     "example.com.",
			want:       "example.com.",
		},
		{
			testName:   "relative subdomain is appended to domain",
			recordName: "www",
			domain:     "example.com.",
			want:       "www.example.com.",
		},
		{
			testName:   "fqdn name returned as-is",
			recordName: "absolute.example.com.",
			domain:     "example.com.",
			want:       "absolute.example.com.",
		},
		{
			testName:   "wildcard subdomain is appended to domain",
			recordName: "*.test",
			domain:     "example.com.",
			want:       "*.test.example.com.",
		},
		{
			testName:   "wildcard fqdn returned as-is",
			recordName: "*.example.com.",
			domain:     "example.com.",
			want:       "*.example.com.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			got := rm.getDNSName(tt.recordName, tt.domain)
			if got != tt.want {
				t.Errorf("getDNSName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// callsGetChange reports whether syncStatus proceeded past its short-circuit
// checks to the GetChange API call. rm is constructed with a nil sdkapi, so
// reaching the API call panics on the nil pointer dereference; we recover that
// panic and treat it as "the API path was taken". A clean return means
// syncStatus short-circuited without polling GetChange.
func callsGetChange(ko *svcapitypes.RecordSet) (called bool) {
	rm := &resourceManager{}
	defer func() {
		if recover() != nil {
			called = true
		}
	}()
	_ = rm.syncStatus(context.Background(), ko)
	return false
}

func Test_syncStatus_skipsPollWhenInsync(t *testing.T) {
	tests := []struct {
		testName        string
		id              *string
		status          *string
		wantCallsChange bool
	}{
		{
			testName:        "no change ID skips poll",
			id:              nil,
			status:          aws.String("INSYNC"),
			wantCallsChange: false,
		},
		{
			testName:        "INSYNC change is terminal and skips poll",
			id:              aws.String("/change/C1234567890"),
			status:          aws.String("INSYNC"),
			wantCallsChange: false,
		},
		{
			testName:        "PENDING change is re-polled",
			id:              aws.String("/change/C1234567890"),
			status:          aws.String("PENDING"),
			wantCallsChange: true,
		},
		{
			testName:        "nil status with change ID is re-polled",
			id:              aws.String("/change/C1234567890"),
			status:          nil,
			wantCallsChange: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			ko := &svcapitypes.RecordSet{}
			ko.Status.ID = tt.id
			ko.Status.Status = tt.status

			if got := callsGetChange(ko); got != tt.wantCallsChange {
				t.Errorf("syncStatus polled GetChange = %v, want %v", got, tt.wantCallsChange)
			}
		})
	}
}

// changeBatchErr is a minimal smithy.APIError implementation used to exercise
// requeueOnTransientChangeBatchError without a live Route53 client.
type changeBatchErr struct {
	code string
	msg  string
}

func (e *changeBatchErr) Error() string        { return e.code + ": " + e.msg }
func (e *changeBatchErr) ErrorCode() string    { return e.code }
func (e *changeBatchErr) ErrorMessage() string { return e.msg }
func (e *changeBatchErr) ErrorFault() smithy.ErrorFault {
	return smithy.FaultClient
}

func Test_requeueOnTransientChangeBatchError(t *testing.T) {
	tests := []struct {
		testName    string
		in          error
		wantRequeue bool // true => downgraded to *RequeueNeededAfter; false => returned unchanged
	}{
		{
			testName:    "nil error stays nil",
			in:          nil,
			wantRequeue: false,
		},
		{
			testName: "transient: record already exists is requeued",
			in: &changeBatchErr{
				code: "InvalidChangeBatch",
				msg:  "[Tried to create resource record set [name='www.example.com.', type='A'] but it already exists]",
			},
			wantRequeue: true,
		},
		{
			testName: "transient: missing alias target is requeued",
			in: &changeBatchErr{
				code: "InvalidChangeBatch",
				msg:  "[Tried to create an alias that targets d123.elb.amazonaws.com., type A in zone Z1, but the alias target name does not lie within the target zone]",
			},
			wantRequeue: true,
		},
		{
			testName: "terminal: malformed InvalidChangeBatch stays terminal",
			in: &changeBatchErr{
				code: "InvalidChangeBatch",
				msg:  "[RRSet with DNS name www.example.com. is not permitted in zone example.org.]",
			},
			wantRequeue: false,
		},
		{
			testName: "non-InvalidChangeBatch code is returned unchanged",
			in: &changeBatchErr{
				code: "InvalidInput",
				msg:  "some other problem",
			},
			wantRequeue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			got := requeueOnTransientChangeBatchError(tt.in)
			var requeued *ackrequeue.RequeueNeededAfter
			isRequeue := errors.As(got, &requeued)
			if isRequeue != tt.wantRequeue {
				t.Errorf("requeueOnTransientChangeBatchError() requeued = %v, want %v (err=%v)",
					isRequeue, tt.wantRequeue, got)
			}
			// When not downgraded, the original error must pass through untouched.
			if !tt.wantRequeue && got != nil && !errors.Is(got, tt.in) {
				t.Errorf("expected original error to pass through unchanged, got %v", got)
			}
		})
	}
}
