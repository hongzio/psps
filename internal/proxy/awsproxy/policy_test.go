package awsproxy

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Ported from isolate's test_awscreds.py. The session policy is the
// security boundary here: AWS enforces it, but only if this package builds
// it correctly. The subtlety worth a regression test was measured before
// isolate wrote it: ssm:DescribeInstanceInformation authorizes against
// arn:aws:ssm:region:account:*, not an instance ARN, so it lives in its own
// statement.

const (
	jp     = "i-0a66b609cd50bb881"
	kr     = "i-0fc2ac4d382c2c6b4"
	bucket = "axis-tre-ansible-ssm-transfer-dev"
)

func statements(t *testing.T, instances, buckets []string) map[string]Statement {
	t.Helper()
	p, err := SessionPolicy(instances, buckets)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Statement{}
	for _, s := range p.Statement {
		out[s.Sid] = s
	}
	return out
}

func ec2(id string) string { return "arn:aws:ec2:*:*:instance/" + id }

func mustDeny(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("accepted")
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%q does not mention %q", err, want)
	}
}

func TestAnEmptyAllowlistIsRefusedNotEncoded(t *testing.T) {
	// A policy with no statements is not valid IAM; the refusal happens here
	// with a reason instead of as a cryptic STS validation error.
	_, err := SessionPolicy(nil, nil)
	mustDeny(t, err, "--allow-instance")
}

func TestSessionsArePinnedToTheNamedInstances(t *testing.T) {
	s := statements(t, []string{jp, kr}, nil)["Sessions"]
	if !reflect.DeepEqual(s.Action, []string{"ssm:StartSession"}) {
		t.Fatalf("actions = %v", s.Action)
	}
	if !slices.Contains(s.Resource, ec2(jp)) || !slices.Contains(s.Resource, ec2(kr)) {
		t.Fatalf("resources = %v", s.Resource)
	}
	// The broad ssm resource must not ride in this statement.
	if slices.Contains(s.Resource, "arn:aws:ssm:*:*:*") {
		t.Fatal("the broad ssm resource rides with the session actions")
	}
}

func TestABareInstanceGetsBothSessionsAndCommands(t *testing.T) {
	s := statements(t, []string{jp}, nil)
	if !slices.Contains(s["Sessions"].Resource, ec2(jp)) {
		t.Fatal("no session")
	}
	if !reflect.DeepEqual(s["Commands"].Action, []string{"ssm:SendCommand"}) || !slices.Contains(s["Commands"].Resource, ec2(jp)) {
		t.Fatalf("commands = %+v", s["Commands"])
	}
}

func TestASessionOnlyInstanceCannotSendCommands(t *testing.T) {
	s := statements(t, []string{jp + "=session", kr}, nil)
	if !slices.Contains(s["Sessions"].Resource, ec2(jp)) {
		t.Fatal("no session")
	}
	if slices.Contains(s["Commands"].Resource, ec2(jp)) {
		t.Fatal("a session-only instance can SendCommand")
	}
	if !slices.Contains(s["Commands"].Resource, ec2(kr)) {
		t.Fatal("the bare instance lost its commands")
	}
}

func TestACommandOnlyInstanceOpensNoSession(t *testing.T) {
	s := statements(t, []string{jp + "=command"}, nil)
	if _, ok := s["Sessions"]; ok {
		t.Fatal("a command-only instance opens sessions")
	}
	// No session capability anywhere means nothing to terminate either.
	if _, ok := s["SessionLifecycle"]; ok {
		t.Fatal("TerminateSession without sessions")
	}
	if !slices.Contains(s["Commands"].Resource, ec2(jp)) {
		t.Fatal("no commands")
	}
}

func TestAReadOnlyBucketGetsNoWriteOrDelete(t *testing.T) {
	s := statements(t, nil, []string{bucket + "=read"})
	read := s["TransferRead"]
	if got := slices.Sorted(slices.Values(read.Action)); !reflect.DeepEqual(got, []string{"s3:GetObject", "s3:ListBucket"}) {
		t.Fatalf("read actions = %v", got)
	}
	if !slices.Contains(read.Resource, "arn:aws:s3:::"+bucket+"/*") {
		t.Fatalf("read resources = %v", read.Resource)
	}
	for _, sid := range []string{"TransferWrite", "TransferDelete"} {
		if _, ok := s[sid]; ok {
			t.Fatalf("%s for a read-only bucket", sid)
		}
	}
}

func TestBucketCapsGroupByCapabilityNotByBucket(t *testing.T) {
	other := "another-transfer-bucket"
	s := statements(t, nil, []string{bucket + "=read,write", other + "=read"})
	obj := func(b string) string { return "arn:aws:s3:::" + b + "/*" }
	if !slices.Contains(s["TransferRead"].Resource, obj(bucket)) || !slices.Contains(s["TransferRead"].Resource, obj(other)) {
		t.Fatalf("read = %v", s["TransferRead"].Resource)
	}
	if !slices.Contains(s["TransferWrite"].Resource, obj(bucket)) || slices.Contains(s["TransferWrite"].Resource, obj(other)) {
		t.Fatalf("write = %v", s["TransferWrite"].Resource)
	}
}

func TestAnUnknownCapabilityIsRefusedWithTheVocabulary(t *testing.T) {
	_, err := SessionPolicy([]string{jp + "=shell"}, nil)
	mustDeny(t, err, "session")
	_, err = SessionPolicy(nil, []string{bucket + "=append"})
	mustDeny(t, err, "read")
}

func TestSessionDocumentsAreAllowedAlongsideTheInstances(t *testing.T) {
	// StartSession authorizes against the document too; without these the
	// allowed instance still refuses to open.
	s := statements(t, []string{jp}, nil)["Sessions"]
	for _, doc := range []string{"arn:aws:ssm:*:*:document/*", "arn:aws:ssm:*::document/AWS-RunShellScript"} {
		if !slices.Contains(s.Resource, doc) {
			t.Fatalf("%s missing from %v", doc, s.Resource)
		}
	}
}

func TestResourceUnspecificReadsGetTheirOwnStatement(t *testing.T) {
	s := statements(t, []string{jp}, nil)
	reads := s["SsmReads"]
	if !slices.Contains(reads.Action, "ssm:DescribeInstanceInformation") || !slices.Contains(reads.Action, "ssm:GetCommandInvocation") {
		t.Fatalf("reads = %v", reads.Action)
	}
	if !reflect.DeepEqual(reads.Resource, []string{"arn:aws:ssm:*:*:*"}) {
		t.Fatalf("read resources = %v", reads.Resource)
	}
	life := s["SessionLifecycle"]
	if !slices.Contains(life.Action, "ssm:TerminateSession") || !reflect.DeepEqual(life.Resource, []string{"arn:aws:ssm:*:*:session/*"}) {
		t.Fatalf("lifecycle = %+v", life)
	}
}

func TestABareBucketGetsReadWriteAndDelete(t *testing.T) {
	s := statements(t, nil, []string{bucket})
	if !slices.Contains(s["TransferRead"].Resource, "arn:aws:s3:::"+bucket) || !slices.Contains(s["TransferRead"].Resource, "arn:aws:s3:::"+bucket+"/*") {
		t.Fatalf("read = %v", s["TransferRead"].Resource)
	}
	if !reflect.DeepEqual(s["TransferWrite"].Action, []string{"s3:PutObject"}) || !reflect.DeepEqual(s["TransferDelete"].Action, []string{"s3:DeleteObject"}) {
		t.Fatalf("write/delete = %+v %+v", s["TransferWrite"], s["TransferDelete"])
	}
}

func TestInstancesOnlyEmitsNoS3StatementAndViceVersa(t *testing.T) {
	if _, ok := statements(t, []string{jp}, nil)["TransferRead"]; ok {
		t.Fatal("s3 statement without buckets")
	}
	s3 := statements(t, nil, []string{bucket})
	for _, sid := range []string{"Sessions", "SsmReads"} {
		if _, ok := s3[sid]; ok {
			t.Fatalf("%s without instances", sid)
		}
	}
}

func TestAMalformedInstanceIDIsRefused(t *testing.T) {
	for _, bad := range []string{"i-XYZ", "arn:aws:ec2:*:*:instance/*", "i-", "*", "i-0a66/../x"} {
		if _, err := SessionPolicy([]string{bad}, nil); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAMalformedBucketNameIsRefused(t *testing.T) {
	for _, bad := range []string{"Has_Caps", "a", "*", "bucket/with/slash", "-leading"} {
		if _, err := SessionPolicy(nil, []string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestAnOversizedPolicyIsRefusedWithTheLimitNamed(t *testing.T) {
	var many []string
	for n := range 80 {
		many = append(many, fmt.Sprintf("i-%017d", n))
	}
	_, err := SessionPolicy(many, nil)
	mustDeny(t, err, "2048")
}

func TestAFullCapabilitySetCollapsesToTheBareName(t *testing.T) {
	for entry, kind := range map[string]Kind{jp + "=command,session": Instance, bucket + "=delete,read,write": Bucket} {
		got, err := CanonicalEntry(entry, kind)
		if err != nil || got != strings.SplitN(entry, "=", 2)[0] {
			t.Errorf("CanonicalEntry(%q) = %q, %v", entry, got, err)
		}
	}
}

func TestPartialCapabilitiesAreSortedAndKept(t *testing.T) {
	for entry, want := range map[string]string{jp + "=session": jp + "=session", bucket + "=write,read": bucket + "=read,write"} {
		kind := Instance
		if !strings.HasPrefix(entry, "i-") {
			kind = Bucket
		}
		if got, err := CanonicalEntry(entry, kind); err != nil || got != want {
			t.Errorf("CanonicalEntry(%q) = %q, %v", entry, got, err)
		}
	}
}

func TestDuplicateAndEmptyCapsCollapse(t *testing.T) {
	if got, _ := CanonicalEntry(bucket+"=read,,read", Bucket); got != bucket+"=read" {
		t.Fatalf("got %q", got)
	}
}

func TestBadNamesAndCapsAreRefused(t *testing.T) {
	for _, c := range []struct {
		entry string
		kind  Kind
	}{{jp + "=shell", Instance}, {"*=read", Bucket}, {jp + "=", Instance}} {
		if _, err := CanonicalEntry(c.entry, c.kind); err == nil {
			t.Errorf("%q accepted", c.entry)
		}
	}
}

func TestSameNameCapabilitiesUnion(t *testing.T) {
	// Two partial spellings of one instance mean the union, and the union
	// here is the full set, which collapses to the bare name.
	got, err := MergeEntries([]string{jp + "=session", jp + "=command"}, Instance)
	if err != nil || !reflect.DeepEqual(got, []string{jp}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestDistinctNamesStaySortedAndCanonical(t *testing.T) {
	got, err := MergeEntries([]string{bucket + "=write,read", "a-bucket"}, Bucket)
	if err != nil || !reflect.DeepEqual(got, []string{"a-bucket", bucket + "=read,write"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func batchStatements(t *testing.T, a Allowlist) map[string]Statement {
	t.Helper()
	p, err := a.Policy()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Statement{}
	for _, s := range p.Statement {
		out[s.Sid] = s
	}
	return out
}

func TestASubmissionIsPinnedToTheNamedQueuesAndDefinitions(t *testing.T) {
	// SubmitJob authorizes against the queue, the definition and the new
	// job together: the queue and the definition are what pin it, so
	// neither may be a wildcard, and every revision of a definition is in.
	s := batchStatements(t, Allowlist{BatchQueues: []string{"mtlab-eval", "mtlab-dev"}, BatchDefinitions: []string{"mtlab-replay-dev"}})
	sub := s["BatchSubmit"]
	if !reflect.DeepEqual(sub.Action, []string{"batch:SubmitJob"}) {
		t.Fatalf("actions = %v", sub.Action)
	}
	want := []string{
		"arn:aws:batch:*:*:job-queue/mtlab-dev", "arn:aws:batch:*:*:job-queue/mtlab-eval",
		"arn:aws:batch:*:*:job-definition/mtlab-replay-dev", "arn:aws:batch:*:*:job-definition/mtlab-replay-dev:*",
		"arn:aws:batch:*:*:job/*",
	}
	if !reflect.DeepEqual(sub.Resource, want) {
		t.Fatalf("resources = %v", sub.Resource)
	}
	for _, r := range sub.Resource {
		if strings.HasSuffix(r, "job-queue/*") || strings.HasSuffix(r, "job-definition/*") {
			t.Fatalf("a wildcard queue or definition: %s", r)
		}
	}
	// The job actions and reads are in statements of their own, so the
	// breadth they need never reaches SubmitJob.
	if !reflect.DeepEqual(s["BatchJobs"].Resource, []string{"arn:aws:batch:*:*:job/*"}) || slices.Contains(s["BatchJobs"].Action, "batch:SubmitJob") {
		t.Fatalf("job actions = %+v", s["BatchJobs"])
	}
	if slices.Contains(s["BatchReads"].Action, "batch:SubmitJob") || !slices.Contains(s["BatchReads"].Action, "batch:DescribeJobs") {
		t.Fatalf("reads = %+v", s["BatchReads"])
	}
	// Nothing else rides along: no S3, no SSM.
	if len(s) != 3 {
		t.Fatalf("statements = %v", s)
	}
}

func TestAQueueWithoutADefinitionIsRefused(t *testing.T) {
	_, err := Allowlist{BatchQueues: []string{"mtlab-eval"}}.Policy()
	mustDeny(t, err, "--allow-batch-definition")
	_, err = Allowlist{Buckets: []string{bucket}, BatchDefinitions: []string{"mtlab-replay-dev"}}.Policy()
	mustDeny(t, err, "--allow-batch-queue")
}

func TestABatchOrLogNameCannotCarryAWildcard(t *testing.T) {
	for _, a := range []Allowlist{
		{BatchQueues: []string{"*"}, BatchDefinitions: []string{"d"}},
		{BatchQueues: []string{"q"}, BatchDefinitions: []string{"mtlab-*"}},
		{BatchQueues: []string{"q"}, BatchDefinitions: []string{"d:3"}},
		{LogGroups: []string{"/mtlab/*"}},
		{LogGroups: []string{"/mtlab/replay:log-stream:x"}},
	} {
		if _, err := a.Policy(); err == nil {
			t.Fatalf("accepted %+v", a)
		}
	}
}

func TestALogGroupIsReadStreamByStream(t *testing.T) {
	s := batchStatements(t, Allowlist{LogGroups: []string{"/mtlab/replay"}})
	if !reflect.DeepEqual(s["LogReads"], Statement{Sid: "LogReads", Effect: "Allow", Action: []string{"logs:GetLogEvents"},
		Resource: []string{"arn:aws:logs:*:*:log-group:/mtlab/replay:log-stream:*"}}) {
		t.Fatalf("logs = %+v", s["LogReads"])
	}
}

func TestStrictModeKeepsBatch(t *testing.T) {
	p, err := strictPolicy(Allowlist{Instances: []string{jp}, BatchQueues: []string{"q"}, BatchDefinitions: []string{"d"}})
	if err != nil {
		t.Fatal(err)
	}
	var sids []string
	for _, s := range p.Statement {
		sids = append(sids, s.Sid)
	}
	if !slices.Contains(sids, "BatchSubmit") || slices.Contains(sids, "Sessions") {
		t.Fatalf("statements = %v", sids)
	}
}

func TestTheMTLabSimAllowlistFitsSTS(t *testing.T) {
	_, err := Allowlist{Buckets: []string{"mtlab-work", "mtlab-tapes=read"},
		BatchQueues: []string{"mtlab-eval", "mtlab-dev"}, BatchDefinitions: []string{"mtlab-replay-dev", "mtlab-replay-eval"},
		LogGroups: []string{"/mtlab/replay"}}.Policy()
	if err != nil {
		t.Fatal(err)
	}
}
