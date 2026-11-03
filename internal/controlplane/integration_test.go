package controlplane

// End-to-end tests of the control plane against DynamoDB Local plus fake
// EC2/SQS/SSM/Pricing and GitHub servers:
//
//	docker run -d -p 8000:8000 amazon/dynamodb-local
//	TRUNER_DYNAMODB_LOCAL=http://localhost:8000 go test ./internal/controlplane -run Integration -v

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/truner/truner/internal/agentcfg"
	"github.com/truner/truner/internal/github"
)

type fakeInstance struct {
	id, state, itype, lifecycle string
	tags                        map[string]string
}

type fakeAWS struct {
	ddb       http.Handler
	mu        sync.Mutex
	params    map[string]string
	messages  []string
	lts       map[string]url.Values
	fleets    []url.Values
	deletedLT []string
	instances map[string]*fakeInstance
	order     []string
	spotFail  bool
	nextID    int
	objects   map[string][]byte // S3, keyed by "bucket/key"
	images    map[string]string // AMI id -> state
}

func (f *fakeAWS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")
	if strings.HasPrefix(target, "DynamoDB_") {
		f.ddb.ServeHTTP(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(target, "AmazonSSM."):
		var in struct{ Name string }
		json.Unmarshal(body, &in)
		v, ok := f.params[in.Name]
		if !ok {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"__type":"ParameterNotFound","message":"not found"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Parameter": map[string]any{"Name": in.Name, "Value": v, "Type": "SecureString"}})
	case target == "AmazonSQS.SendMessage":
		var in struct{ MessageBody string }
		json.Unmarshal(body, &in)
		f.messages = append(f.messages, in.MessageBody)
		sum := md5.Sum([]byte(in.MessageBody))
		json.NewEncoder(w).Encode(map[string]string{"MessageId": fmt.Sprint(len(f.messages)), "MD5OfMessageBody": hex.EncodeToString(sum[:])})
	case strings.HasPrefix(target, "AWSPriceListService."):
		doc := `{"terms":{"OnDemand":{"x":{"priceDimensions":{"y":{"pricePerUnit":{"USD":"0.1500"}}}}}}}`
		json.NewEncoder(w).Encode(map[string]any{"PriceList": []string{doc}, "FormatVersion": "aws_v1"})
	case r.Method == http.MethodPut || r.Method == http.MethodGet || r.Method == http.MethodHead:
		key := strings.TrimPrefix(r.URL.Path, "/")
		if r.Method == http.MethodPut {
			f.objects[key] = body
			w.Header().Set("ETag", `"x"`)
			return
		}
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			fmt.Fprint(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>no</Message></Error>`)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Write(data)
	default:
		form, _ := url.ParseQuery(string(body))
		if a := form.Get("Action"); a != "" {
			f.ec2(w, a, form)
			return
		}
		w.WriteHeader(404)
	}
}

var filterNameRe = regexp.MustCompile(`^Filter\.(\d+)\.Name$`)

var tagKeyRe = regexp.MustCompile(`^LaunchTemplateData\.TagSpecification\.1\.Tag\.(\d+)\.Key$`)

func (f *fakeAWS) ec2(w http.ResponseWriter, action string, form url.Values) {
	const ns = `xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"`
	write := func(s string) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<%sResponse %s><requestId>r</requestId>%s</%sResponse>`, action, ns, s, action)
	}
	switch action {
	case "CreateLaunchTemplate":
		f.nextID++
		id := fmt.Sprintf("lt-%d", f.nextID)
		f.lts[id] = form
		write(`<launchTemplate><launchTemplateId>` + id + `</launchTemplateId></launchTemplate>`)
	case "DeleteLaunchTemplate":
		f.deletedLT = append(f.deletedLT, form.Get("LaunchTemplateId"))
		write(`<launchTemplate><launchTemplateId>` + form.Get("LaunchTemplateId") + `</launchTemplateId></launchTemplate>`)
	case "CreateFleet":
		f.fleets = append(f.fleets, form)
		kind := form.Get("TargetCapacitySpecification.DefaultTargetCapacityType")
		if kind == "spot" && f.spotFail {
			write(`<fleetId>fleet-x</fleetId><errorSet><item><errorCode>InsufficientInstanceCapacity</errorCode><errorMessage>no spot</errorMessage><lifecycle>spot</lifecycle></item></errorSet><fleetInstanceSet/>`)
			return
		}
		lt := f.lts[form.Get("LaunchTemplateConfigs.1.LaunchTemplateSpecification.LaunchTemplateId")]
		tags := map[string]string{}
		for k := range lt {
			if m := tagKeyRe.FindStringSubmatch(k); m != nil {
				tags[lt.Get(k)] = lt.Get("LaunchTemplateData.TagSpecification.1.Tag." + m[1] + ".Value")
			}
		}
		f.nextID++
		id := fmt.Sprintf("i-%04d", f.nextID)
		lc := "spot"
		if kind == "on-demand" {
			lc = "on-demand"
		}
		f.instances[id] = &fakeInstance{id: id, state: "running", itype: "c7a.xlarge", lifecycle: lc, tags: tags}
		f.order = append(f.order, id)
		lcXML := ""
		if lc == "spot" {
			lcXML = "<lifecycle>spot</lifecycle>"
		}
		write(`<fleetId>fleet-1</fleetId><fleetInstanceSet><item><instanceIds><item>` + id + `</item></instanceIds><instanceType>c7a.xlarge</instanceType>` + lcXML +
			`<launchTemplateAndOverrides><overrides><subnetId>subnet-1</subnetId><availabilityZone>us-east-1a</availabilityZone></overrides></launchTemplateAndOverrides></item></fleetInstanceSet><errorSet/>`)
	case "DescribeImages":
		if st, ok := f.images[form.Get("ImageId.1")]; ok {
			write(`<imagesSet><item><imageId>` + form.Get("ImageId.1") + `</imageId><imageState>` + st + `</imageState><architecture>x86_64</architecture><rootDeviceName>/dev/sda1</rootDeviceName></item></imagesSet>`)
			return
		}
		if form.Get("Owner.1") == "self" {
			write(`<imagesSet/>`)
			return
		}
		write(`<imagesSet><item><imageId>ami-base</imageId><architecture>x86_64</architecture><rootDeviceName>/dev/sda1</rootDeviceName><imageState>available</imageState>` +
			`<creationDate>2026-10-01T00:00:00.000Z</creationDate><platformDetails>Linux/UNIX</platformDetails><blockDeviceMapping><item><deviceName>/dev/sda1</deviceName><ebs><volumeSize>8</volumeSize></ebs></item></blockDeviceMapping></item></imagesSet>`)
	case "DescribeInstances":
		want := map[string]bool{}
		filters := map[string][]string{}
		for k, v := range form {
			if strings.HasPrefix(k, "InstanceId.") {
				want[v[0]] = true
			}
			if m := filterNameRe.FindStringSubmatch(k); m != nil {
				for i := 1; form.Get(fmt.Sprintf("Filter.%s.Value.%d", m[1], i)) != ""; i++ {
					filters[v[0]] = append(filters[v[0]], form.Get(fmt.Sprintf("Filter.%s.Value.%d", m[1], i)))
				}
			}
		}
		match := func(in *fakeInstance) bool {
			for name, vals := range filters {
				got := in.state
				if strings.HasPrefix(name, "tag:") {
					got = in.tags[strings.TrimPrefix(name, "tag:")]
				}
				ok := false
				for _, v := range vals {
					ok = ok || v == got
				}
				if !ok {
					return false
				}
			}
			return true
		}
		var b strings.Builder
		for _, id := range f.order {
			in := f.instances[id]
			if len(want) > 0 && !want[id] {
				continue
			}
			if len(want) == 0 && (in.state == "terminated" || !match(in)) {
				continue
			}
			b.WriteString(`<item><reservationId>r-1</reservationId><instancesSet><item><instanceId>` + id + `</instanceId><instanceType>` + in.itype + `</instanceType><instanceState><code>16</code><name>` + in.state + `</name></instanceState><launchTime>` + time.Now().UTC().Format("2006-01-02T15:04:05.000Z") + `</launchTime><tagSet>`)
			for k, v := range in.tags {
				b.WriteString(`<item><key>` + k + `</key><value>` + xmlEsc(v) + `</value></item>`)
			}
			b.WriteString(`</tagSet></item></instancesSet></item>`)
		}
		write(`<reservationSet>` + b.String() + `</reservationSet>`)
	case "DescribeSpotPriceHistory":
		write(`<spotPriceHistorySet><item><instanceType>c7a.xlarge</instanceType><productDescription>Linux/UNIX</productDescription><spotPrice>0.0500</spotPrice><availabilityZone>us-east-1a</availabilityZone></item></spotPriceHistorySet>`)
	case "TerminateInstances":
		for k, v := range form {
			if strings.HasPrefix(k, "InstanceId.") {
				if in := f.instances[v[0]]; in != nil {
					in.state = "terminated"
				}
			}
		}
		write(`<instancesSet/>`)
	case "DescribeLaunchTemplates":
		write(`<launchTemplates/>`)
	case "CreateTags":
		for k, v := range form {
			if strings.HasPrefix(k, "ResourceId.") {
				if in := f.instances[v[0]]; in != nil {
					for i := 1; form.Get(fmt.Sprintf("Tag.%d.Key", i)) != ""; i++ {
						in.tags[form.Get(fmt.Sprintf("Tag.%d.Key", i))] = form.Get(fmt.Sprintf("Tag.%d.Value", i))
					}
				}
			}
		}
		write(`<return>true</return>`)
	case "StartInstances":
		if in := f.instances[form.Get("InstanceId.1")]; in != nil {
			in.state = "running"
		}
		write(`<instancesSet/>`)
	case "ModifyInstanceAttribute":
		write(`<return>true</return>`)
	case "CreateImage":
		f.nextID++
		id := fmt.Sprintf("ami-baked%d", f.nextID)
		f.images[id] = "pending"
		write(`<imageId>` + id + `</imageId>`)
	default:
		w.WriteHeader(400)
		fmt.Fprintf(w, `<Response><Errors><Error><Code>Unsupported</Code><Message>%s</Message></Error></Errors><RequestID>r</RequestID></Response>`, action)
	}
}

func xmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

type fakeGitHub struct {
	mu        sync.Mutex
	jitNames  []string
	jobStatus map[int64]string
	repoCfg   string
	deleted   int
}

func (g *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("POST /repos/{o}/{r}/actions/runners/generate-jitconfig", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name string }
		json.NewDecoder(r.Body).Decode(&in)
		g.mu.Lock()
		g.jitNames = append(g.jitNames, in.Name)
		g.mu.Unlock()
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"runner": map[string]any{"id": 77, "name": in.Name}, "encoded_jit_config": "JITCFG-" + in.Name})
	})
	mux.HandleFunc("GET /repos/{o}/{r}/contents/.github/{file}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.repoCfg == "" || r.PathValue("file") != "truner.yml" {
			http.Error(w, `{"message":"Not Found"}`, 404)
			return
		}
		w.Write([]byte(g.repoCfg))
	})
	mux.HandleFunc("GET /repos/actions/runner/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": "v2.338.2"})
	})
	mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "account": map[string]any{"login": "acme"}}})
	})
	mux.HandleFunc("GET /repos/{o}/{r}/actions/runners/{id}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": 77, "busy": false, "status": "online"})
	})
	mux.HandleFunc("DELETE /repos/{o}/{r}/actions/runners/{id}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.deleted++
		g.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /repos/{o}/{r}/actions/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var id int64
		fmt.Sscan(r.PathValue("id"), &id)
		g.mu.Lock()
		st := g.jobStatus[id]
		g.mu.Unlock()
		if st == "" {
			st = "queued"
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "status": st})
	})
	return mux
}

type harness struct {
	t      *testing.T
	cp     *ControlPlane
	aws    *fakeAWS
	gh     *fakeGitHub
	secret string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	local := os.Getenv("TRUNER_DYNAMODB_LOCAL")
	if local == "" {
		t.Skip("set TRUNER_DYNAMODB_LOCAL (e.g. http://localhost:8000) to run control plane integration tests")
	}
	ctx := context.Background()
	u, _ := url.Parse(local)
	fa := &fakeAWS{ddb: httputil.NewSingleHostReverseProxy(u), params: map[string]string{}, lts: map[string]url.Values{}, instances: map[string]*fakeInstance{}, objects: map[string][]byte{}, images: map[string]string{}}
	awsSrv := httptest.NewServer(fa)
	t.Cleanup(awsSrv.Close)
	fg := &fakeGitHub{jobStatus: map[int64]string{}}
	ghSrv := httptest.NewServer(fg.handler())
	t.Cleanup(ghSrv.Close)
	t.Setenv("TRUNER_GITHUB_API", ghSrv.URL)
	t.Setenv("TRUNER_S3_PATH_STYLE", "true")

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	secret := "s3cret"
	creds, _ := json.Marshal(github.AppCredentials{AppID: 42, Slug: "truner-test", WebhookSecret: secret, PrivateKeyPEM: pemKey})
	fa.params["/truner/test/github-app"] = string(creds)
	fa.params["/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id"] = "ami-base"

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("AKID", "SECRET", "")),
		awsconfig.WithBaseEndpoint(awsSrv.URL))
	if err != nil {
		t.Fatal(err)
	}
	table := fmt.Sprintf("truner-it-%d", time.Now().UnixNano())
	ddb := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(local) })
	_, err = ddb.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   &table,
		BillingMode: ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("coll"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: aws.String("ts"), AttributeType: ddbtypes.ScalarAttributeTypeN},
		},
		KeySchema: []ddbtypes.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: ddbtypes.KeyTypeHash}},
		GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndex{{
			IndexName:  aws.String("byColl"),
			KeySchema:  []ddbtypes.KeySchemaElement{{AttributeName: aws.String("coll"), KeyType: ddbtypes.KeyTypeHash}, {AttributeName: aws.String("ts"), KeyType: ddbtypes.KeyTypeRange}},
			Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
		}},
	})
	if err != nil {
		t.Fatalf("create table (is DynamoDB Local running at %s?): %v", local, err)
	}
	t.Cleanup(func() { ddb.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: &table}) })

	env := &Env{
		Stack: "test", Region: "us-east-1", Bucket: "data", ArtifactsBucket: "artifacts", Table: table,
		QueueURL: awsSrv.URL + "/queue", AppParam: "/truner/test/github-app", PublicSubnets: []string{"subnet-1", "subnet-2"},
		SecurityGroup: "sg-1", InstanceProfileArn: "arn:aws:iam::1:instance-profile/r", LabelPrefix: "truner", Version: "v-test",
		ECRRepoURI: "123.dkr.ecr.us-east-1.amazonaws.com/cache",
	}
	cp := NewWithConfig(env, cfg)
	return &harness{t: t, cp: cp, aws: fa, gh: fg, secret: secret}
}

func (h *harness) webhook(action string, jobID int64, labels []string, runner string, conclusion string) WebhookResponse {
	h.t.Helper()
	now := time.Now().UTC()
	ev := map[string]any{
		"action": action,
		"workflow_job": map[string]any{
			"id": jobID, "run_id": 900, "run_attempt": 1, "name": "build", "workflow_name": "CI", "head_sha": "abc",
			"labels": labels, "runner_name": runner, "conclusion": conclusion,
			"created_at": now.Add(-5 * time.Second), "started_at": now.Add(-3 * time.Second), "completed_at": now,
		},
		"repository":   map[string]any{"name": "app", "full_name": "acme/app", "owner": map[string]any{"login": "acme"}},
		"installation": map[string]any{"id": 1},
		"sender":       map[string]any{"login": "octocat"},
	}
	body, _ := json.Marshal(ev)
	return h.cp.HandleWebhook(context.Background(), map[string]string{
		"x-github-event": "workflow_job", "x-hub-signature-256": github.Sign(h.secret, body), "x-github-delivery": "d",
	}, body)
}

func (h *harness) drainQueue() {
	h.t.Helper()
	for i := 0; i < 10; i++ {
		h.aws.mu.Lock()
		msgs := h.aws.messages
		h.aws.messages = nil
		h.aws.mu.Unlock()
		if len(msgs) == 0 {
			return
		}
		for _, m := range msgs {
			if err := h.cp.HandleQueueMessage(context.Background(), m); err != nil {
				h.t.Fatalf("worker: %v", err)
			}
		}
	}
}

func (h *harness) job(id int64) *Job {
	h.t.Helper()
	j, err := h.cp.Store.GetJob(context.Background(), id)
	if err != nil || j == nil {
		h.t.Fatalf("job %d: %v %v", id, j, err)
	}
	return j
}

// jobFleets returns the CreateFleet requests for job runners (not image builds).
func (h *harness) jobFleets() []url.Values {
	h.aws.mu.Lock()
	defer h.aws.mu.Unlock()
	var out []url.Values
	for _, f := range h.aws.fleets {
		if isRunnerLT(h.aws.lts[f.Get("LaunchTemplateConfigs.1.LaunchTemplateSpecification.LaunchTemplateId")]) {
			out = append(out, f)
		}
	}
	return out
}

func isRunnerLT(lt url.Values) bool {
	for k := range lt {
		if m := tagKeyRe.FindStringSubmatch(k); m != nil && lt.Get(k) == TagRole &&
			lt.Get("LaunchTemplateData.TagSpecification.1.Tag."+m[1]+".Value") == "runner" {
			return true
		}
	}
	return false
}

func (h *harness) lastUserData() *agentcfg.Config {
	h.t.Helper()
	h.aws.mu.Lock()
	defer h.aws.mu.Unlock()
	var last url.Values
	for i := 1; i <= h.aws.nextID; i++ {
		if lt, ok := h.aws.lts[fmt.Sprintf("lt-%d", i)]; ok && isRunnerLT(lt) {
			last = lt
		}
	}
	ud, err := base64.StdEncoding.DecodeString(last.Get("LaunchTemplateData.UserData"))
	if err != nil {
		h.t.Fatal(err)
	}
	cfg, err := agentcfg.FromUserData(ud)
	if err != nil {
		h.t.Fatalf("user data: %v\n%s", err, ud)
	}
	return cfg
}

func TestIntegrationHappyPath(t *testing.T) {
	h := newHarness(t)
	labels := []string{"truner/cpu=4/ram=16/run=900"}

	if r := h.webhook("queued", 1001, labels, "", ""); r.Status != 202 {
		t.Fatalf("queued: %+v", r)
	}
	if r := h.webhook("queued", 1001, labels, "", ""); r.Status != 202 {
		t.Fatalf("duplicate queued: %+v", r)
	}
	if n := len(h.aws.messages); n != 1 {
		t.Fatalf("duplicate deliveries must enqueue once, got %d messages", n)
	}
	h.drainQueue()
	j := h.job(1001)
	if j.Status != StatusLaunched || j.InstanceID == "" || j.InstanceType != "c7a.xlarge" || j.Lifecycle != "spot" || j.Price != 0.05 || j.RunnerID != 77 {
		t.Fatalf("unexpected launch record: %+v", j)
	}
	if n := len(h.jobFleets()); n != 1 {
		t.Fatalf("expected one runner fleet request, got %d", n)
	}
	if it, _ := h.cp.Store.GetItem(context.Background(), bakePK("ubuntu24", "x64")); it == nil || it.Str("status") != "building" {
		t.Fatalf("first use of ubuntu24 must start an image bake: %v", it)
	}
	fleet := h.jobFleets()[0]
	if fleet.Get("LaunchTemplateConfigs.1.Overrides.1.InstanceRequirements.VCpuCount.Min") != "4" ||
		fleet.Get("LaunchTemplateConfigs.1.Overrides.1.InstanceRequirements.MemoryMiB.Min") != "16384" ||
		fleet.Get("LaunchTemplateConfigs.1.Overrides.2.SubnetId") != "subnet-2" ||
		fleet.Get("SpotOptions.AllocationStrategy") != "price-capacity-optimized" {
		t.Fatalf("unexpected fleet request: %v", fleet)
	}
	if len(h.aws.deletedLT) != 2 {
		t.Fatalf("ephemeral launch template must be deleted, got %v", h.aws.deletedLT)
	}
	ac := h.lastUserData()
	if ac.Mode != agentcfg.ModeRun || !strings.HasPrefix(ac.JITConfig, "JITCFG-truner-1001-") || ac.RunnerVersion != "2.338.2" ||
		!ac.Cache.Enabled || ac.Cache.Prefix != "cache/acme/app/" || ac.ImageOS != "ubuntu24" || ac.ECRRegistry != "123.dkr.ecr.us-east-1.amazonaws.com" ||
		ac.Job.Repo != "acme/app" || ac.FatalError != "" || len(ac.RunnerURLs) == 0 {
		t.Fatalf("unexpected agent config: %+v", ac)
	}
	if h.aws.instances[j.InstanceID].tags[TagJobID] != "1001" {
		t.Fatalf("instance tags missing: %v", h.aws.instances[j.InstanceID].tags)
	}

	if r := h.webhook("in_progress", 1001, labels, j.RunnerName, ""); r.Status != 202 {
		t.Fatalf("in_progress: %+v", r)
	}
	if j := h.job(1001); j.Status != StatusRunning || j.PickedBy != 1001 {
		t.Fatalf("expected running+picked: %+v", j)
	}
	if r := h.webhook("completed", 1001, labels, j.RunnerName, "success"); r.Status != 202 {
		t.Fatalf("completed: %+v", r)
	}
	j = h.job(1001)
	if j.Status != StatusCompleted || j.Conclusion != "success" || j.Cost <= 0 || j.RanOnType != "c7a.xlarge" {
		t.Fatalf("expected completed with cost: %+v", j)
	}
	if spend := h.cp.MonthSpend(context.Background()); spend <= 0 {
		t.Fatalf("spend counter not updated: %v", spend)
	}
}

func TestIntegrationSpotFallbackAndBadSignature(t *testing.T) {
	h := newHarness(t)
	h.aws.spotFail = true
	h.webhook("queued", 2001, []string{"truner", "cpu=2"}, "", "")
	h.drainQueue()
	j := h.job(2001)
	if j.Status != StatusLaunched || j.Lifecycle != "on-demand" || !j.FellBack || j.Price != 0.15 {
		t.Fatalf("expected on-demand fallback: %+v", j)
	}
	if fl := h.jobFleets(); len(fl) != 2 || fl[0].Get("TargetCapacitySpecification.DefaultTargetCapacityType") != "spot" || fl[1].Get("TargetCapacitySpecification.DefaultTargetCapacityType") != "on-demand" {
		t.Fatalf("expected spot then on-demand fleet requests, got %d", len(fl))
	}
	r := h.cp.HandleWebhook(context.Background(), map[string]string{"x-github-event": "workflow_job", "x-hub-signature-256": "sha256=00"}, []byte(`{}`))
	if r.Status != 401 {
		t.Fatalf("bad signature must be rejected: %+v", r)
	}
	if r := h.webhook("queued", 2002, []string{"ubuntu-latest"}, "", ""); r.Status != 200 || len(h.aws.messages) != 0 {
		t.Fatalf("non-truner jobs must be ignored: %+v", r)
	}
}

func TestIntegrationCancelledBeforePickup(t *testing.T) {
	h := newHarness(t)
	h.webhook("queued", 3001, []string{"truner/cpu=2"}, "", "")
	h.drainQueue()
	j := h.job(3001)
	h.webhook("completed", 3001, []string{"truner/cpu=2"}, "", "cancelled")
	if got := h.job(3001); got.Status != StatusCancelled {
		t.Fatalf("expected cancelled: %+v", got)
	}
	if st := h.aws.instances[j.InstanceID].state; st != "terminated" {
		t.Fatalf("idle machine must be terminated, state %s", st)
	}
	if h.gh.deleted == 0 {
		t.Fatal("runner registration must be removed")
	}
}

func TestIntegrationInvalidLabelGetsErrorRunner(t *testing.T) {
	h := newHarness(t)
	h.webhook("queued", 4001, []string{"truner/cpu=banana"}, "", "")
	h.drainQueue()
	if j := h.job(4001); j.Status != StatusLaunched {
		t.Fatalf("expected an error runner to be launched: %+v", j)
	}
	ac := h.lastUserData()
	if !strings.Contains(ac.FatalError, "cpu=banana") {
		t.Fatalf("fatal error should explain the label: %q", ac.FatalError)
	}
}

func TestIntegrationRepoConfigAndRelaunch(t *testing.T) {
	h := newHarness(t)
	h.gh.repoCfg = "runners:\n  big:\n    cpu: 16\n    ram: 64\n    disk: 120\n"
	h.webhook("queued", 5001, []string{"truner/runner=big"}, "", "")
	h.drainQueue()
	fl := h.jobFleets()
	fleet := fl[len(fl)-1]
	if fleet.Get("LaunchTemplateConfigs.1.Overrides.1.InstanceRequirements.VCpuCount.Min") != "16" {
		t.Fatalf("repo runner profile not applied: %v", fleet)
	}
	lt := h.aws.lts[fleet.Get("LaunchTemplateConfigs.1.LaunchTemplateSpecification.LaunchTemplateId")]
	if lt.Get("LaunchTemplateData.BlockDeviceMapping.1.Ebs.VolumeSize") != "120" {
		t.Fatalf("disk size not applied: %v", lt)
	}
	// The machine dies before picking up the job (e.g. spot reclaimed while
	// booting): the janitor must relaunch it on on-demand capacity.
	j := h.job(5001)
	h.aws.mu.Lock()
	h.aws.instances[j.InstanceID].state = "terminated"
	h.aws.mu.Unlock()
	ctx := context.Background()
	h.cp.Store.UpdateJob(ctx, 5001, map[string]any{"launchedAt": time.Now().Add(-5 * time.Minute).Unix()}, nil)
	insts, _ := h.cp.ManagedInstances(ctx)
	if err := h.cp.reconcileJobs(ctx, insts); err != nil {
		t.Fatal(err)
	}
	if j := h.job(5001); j.Status != StatusQueued || !j.ForceOD || j.Attempts != 1 {
		t.Fatalf("expected relaunch with on-demand: %+v", j)
	}
	h.drainQueue()
	if j := h.job(5001); j.Status != StatusLaunched || j.Lifecycle != "on-demand" {
		t.Fatalf("expected on-demand relaunch: %+v", j)
	}
}

func TestIntegrationBudgetBlocksLaunch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.cp.global = nil
	h.cp.Store.AddNumber(ctx, "spend#"+time.Now().UTC().Format("2006-01"), "total", 50, time.Hour)
	h.cp.mu.Lock()
	h.cp.global, h.cp.globalAt = mustGlobal(t, "limits:\n  monthlyBudgetUsd: 40\n"), time.Now()
	h.cp.mu.Unlock()
	h.webhook("queued", 6001, []string{"truner"}, "", "")
	h.aws.mu.Lock()
	msgs := h.aws.messages
	h.aws.messages = nil
	h.aws.mu.Unlock()
	for _, m := range msgs {
		h.cp.HandleQueueMessage(ctx, m)
	}
	j := h.job(6001)
	if j.Status != StatusQueued || !strings.Contains(j.Error, "budget") || j.Attempts != 0 || len(h.jobFleets()) != 0 {
		t.Fatalf("budget must hold the job without launching: %+v", j)
	}
	if len(h.aws.messages) != 1 {
		t.Fatalf("job must be re-queued for later, got %d messages", len(h.aws.messages))
	}
}
