package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Job statuses.
const (
	StatusQueued    = "queued"    // waiting for the worker
	StatusLaunching = "launching" // worker is starting a machine
	StatusLaunched  = "launched"  // machine started, runner not yet busy
	StatusRunning   = "running"   // job in progress
	StatusCompleted = "completed"
	StatusCancelled = "cancelled" // job ended before a runner picked it up
	StatusFailed    = "failed"    // could not launch
	StatusRejected  = "rejected"  // not allowed by access rules
)

// Collections partition the byColl index.
const (
	collBakes    = "bakes"
	collReruns   = "reruns"
	collImageUse = "imageuse"
	// CollForeignPrefix holds jobs whose labels look like truner's but carry
	// another label prefix, for `truner doctor`.
	CollForeignPrefix = "foreignprefix"
)

// ErrConditionFailed is returned when a conditional write did not apply.
var ErrConditionFailed = errors.New("condition failed")

// Job is the record of one workflow job (and of the machine launched for it).
type Job struct {
	PK   string `dynamodbav:"pk"`
	Coll string `dynamodbav:"coll"` // jobs#YYYY-MM-DD
	TS   int64  `dynamodbav:"ts"`   // queued at (unix seconds)

	JobID          int64    `dynamodbav:"jobId"`
	RunID          int64    `dynamodbav:"runId"`
	RunAttempt     int      `dynamodbav:"runAttempt"`
	InstallationID int64    `dynamodbav:"installationId"`
	Owner          string   `dynamodbav:"owner"`
	Repo           string   `dynamodbav:"repo"`
	Name           string   `dynamodbav:"name"`
	Workflow       string   `dynamodbav:"workflow"`
	Branch         string   `dynamodbav:"branch,omitempty"`
	SHA            string   `dynamodbav:"sha,omitempty"`
	Sender         string   `dynamodbav:"sender,omitempty"`
	Labels         []string `dynamodbav:"labels"`
	Private        bool     `dynamodbav:"privateRepo,omitempty"`

	Status    string `dynamodbav:"status"`
	Attempts  int    `dynamodbav:"attempts"`
	Error     string `dynamodbav:"error,omitempty"`
	UpdatedAt int64  `dynamodbav:"updatedAt"`

	// Machine launched on behalf of this job.
	RunnerName   string  `dynamodbav:"runnerName,omitempty"`
	RunnerID     int64   `dynamodbav:"runnerId,omitempty"`
	InstanceID   string  `dynamodbav:"instanceId,omitempty"`
	InstanceType string  `dynamodbav:"instanceType,omitempty"`
	Lifecycle    string  `dynamodbav:"lifecycle,omitempty"` // spot | on-demand
	AZ           string  `dynamodbav:"az,omitempty"`
	Image        string  `dynamodbav:"image,omitempty"`
	AMI          string  `dynamodbav:"ami,omitempty"`
	Platform     string  `dynamodbav:"platform,omitempty"`
	DiskGiB      int     `dynamodbav:"diskGiB,omitempty"`
	Price        float64 `dynamodbav:"price,omitempty"` // USD per hour
	Pool         string  `dynamodbav:"pool,omitempty"`
	KeepMinutes  int     `dynamodbav:"keepMinutes,omitempty"`
	LaunchedAt   int64   `dynamodbav:"launchedAt,omitempty"`
	PickedBy     int64   `dynamodbav:"pickedBy,omitempty"`      // job that ran on this machine
	MachineDone  int64   `dynamodbav:"machineDoneAt,omitempty"` // when the job on this machine ended
	Interrupted  bool    `dynamodbav:"interrupted,omitempty"`
	ForceOD      bool    `dynamodbav:"forceOnDemand,omitempty"`
	NoMicro      bool    `dynamodbav:"noMicro,omitempty"`  // a MicroVM did not run it: use EC2
	FellBack     bool    `dynamodbav:"fellBack,omitempty"` // spot → on-demand
	LaunchMillis int64   `dynamodbav:"launchMillis,omitempty"`

	// Execution of this job (possibly on another job's machine).
	RanOn          string  `dynamodbav:"ranOn,omitempty"`
	RanOnInstance  string  `dynamodbav:"ranOnInstance,omitempty"`
	RanOnType      string  `dynamodbav:"ranOnType,omitempty"`
	RanOnLifecycle string  `dynamodbav:"ranOnLifecycle,omitempty"`
	StartedAt      int64   `dynamodbav:"startedAt,omitempty"`
	CompletedAt    int64   `dynamodbav:"completedAt,omitempty"`
	Conclusion     string  `dynamodbav:"conclusion,omitempty"`
	Cost           float64 `dynamodbav:"cost,omitempty"`
	CPU            int     `dynamodbav:"cpu,omitempty"`

	TTL int64 `dynamodbav:"ttl"`
}

// FullName returns owner/repo.
func (j *Job) FullName() string { return j.Owner + "/" + j.Repo }

// JobPK returns the partition key of a job.
func JobPK(id int64) string { return "job#" + strconv.FormatInt(id, 10) }

// JobColl returns the index collection for a day.
func JobColl(t time.Time) string { return "jobs#" + t.UTC().Format("2006-01-02") }

// Store is the DynamoDB-backed state store.
type Store struct {
	DB    *dynamodb.Client
	Table string
}

// PutJobIfAbsent inserts a job record unless it already exists.
func (s *Store) PutJobIfAbsent(ctx context.Context, j *Job) (bool, error) {
	item, err := attributevalue.MarshalMap(j)
	if err != nil {
		return false, err
	}
	_, err = s.DB.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           &s.Table,
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(pk)"),
	})
	if isConditionFailed(err) {
		return false, nil
	}
	return err == nil, err
}

// PutJob writes a job record unconditionally.
func (s *Store) PutJob(ctx context.Context, j *Job) error {
	item, err := attributevalue.MarshalMap(j)
	if err != nil {
		return err
	}
	_, err = s.DB.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.Table, Item: item})
	return err
}

// GetJob loads a job record, returning (nil, nil) when it does not exist.
func (s *Store) GetJob(ctx context.Context, id int64) (*Job, error) {
	out, err := s.DB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &s.Table,
		Key:            map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: JobPK(id)}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil || out.Item == nil {
		return nil, err
	}
	j := &Job{}
	return j, attributevalue.UnmarshalMap(out.Item, j)
}

// UpdateJob sets fields on a job. cond may be nil.
func (s *Store) UpdateJob(ctx context.Context, id int64, set map[string]any, cond *expression.ConditionBuilder) error {
	set["updatedAt"] = time.Now().Unix()
	var upd expression.UpdateBuilder
	first := true
	for k, v := range set {
		if first {
			upd = expression.Set(expression.Name(k), expression.Value(v))
			first = false
		} else {
			upd = upd.Set(expression.Name(k), expression.Value(v))
		}
	}
	b := expression.NewBuilder().WithUpdate(upd)
	exists := expression.AttributeExists(expression.Name("pk"))
	if cond != nil {
		b = b.WithCondition(exists.And(*cond))
	} else {
		b = b.WithCondition(exists)
	}
	expr, err := b.Build()
	if err != nil {
		return err
	}
	_, err = s.DB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &s.Table,
		Key:                       map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: JobPK(id)}},
		UpdateExpression:          expr.Update(),
		ConditionExpression:       expr.Condition(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
	})
	if isConditionFailed(err) {
		return ErrConditionFailed
	}
	return err
}

// StatusIn builds a condition matching any of the given statuses.
func StatusIn(statuses ...string) *expression.ConditionBuilder {
	var ops []expression.OperandBuilder
	for _, s := range statuses[1:] {
		ops = append(ops, expression.Value(s))
	}
	c := expression.Name("status").In(expression.Value(statuses[0]), ops...)
	return &c
}

// JobsInColl lists all jobs of one day collection.
func (s *Store) JobsInColl(ctx context.Context, coll string) ([]Job, error) {
	var out []Job
	err := s.queryColl(ctx, coll, 0, func(item map[string]ddbtypes.AttributeValue) error {
		var j Job
		if err := attributevalue.UnmarshalMap(item, &j); err != nil {
			return err
		}
		out = append(out, j)
		return nil
	})
	return out, err
}

func (s *Store) queryColl(ctx context.Context, coll string, sinceTS int64, fn func(map[string]ddbtypes.AttributeValue) error) error {
	keyCond := expression.Key("coll").Equal(expression.Value(coll))
	if sinceTS > 0 {
		keyCond = keyCond.And(expression.Key("ts").GreaterThanEqual(expression.Value(sinceTS)))
	}
	expr, err := expression.NewBuilder().WithKeyCondition(keyCond).Build()
	if err != nil {
		return err
	}
	p := dynamodb.NewQueryPaginator(s.DB, &dynamodb.QueryInput{
		TableName:                 &s.Table,
		IndexName:                 aws.String("byColl"),
		KeyConditionExpression:    expr.KeyCondition(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, it := range page.Items {
			if err := fn(it); err != nil {
				return err
			}
		}
	}
	return nil
}

// Item is a generic record (bakes, reruns, counters, markers).
type Item map[string]any

// GetItem loads a generic item; nil when missing.
func (s *Store) GetItem(ctx context.Context, pk string) (Item, error) {
	out, err := s.DB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &s.Table,
		Key:            map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: pk}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil || out.Item == nil {
		return nil, err
	}
	it := Item{}
	return it, attributevalue.UnmarshalMap(out.Item, &it)
}

// PutItem writes a generic item. ttl is a duration from now (0 = none).
// When ifAbsent is set the write only succeeds if the key is new.
func (s *Store) PutItem(ctx context.Context, pk string, it Item, ttl time.Duration, ifAbsent bool) (bool, error) {
	if it == nil {
		it = Item{}
	}
	it["pk"] = pk
	if ttl > 0 {
		it["ttl"] = time.Now().Add(ttl).Unix()
	}
	av, err := attributevalue.MarshalMap(it)
	if err != nil {
		return false, err
	}
	in := &dynamodb.PutItemInput{TableName: &s.Table, Item: av}
	if ifAbsent {
		in.ConditionExpression = aws.String("attribute_not_exists(pk)")
	}
	_, err = s.DB.PutItem(ctx, in)
	if isConditionFailed(err) {
		return false, nil
	}
	return err == nil, err
}

// UpdateItem sets fields of a generic item, creating it if needed.
func (s *Store) UpdateItem(ctx context.Context, pk string, set Item) error {
	var upd expression.UpdateBuilder
	first := true
	for k, v := range set {
		if first {
			upd = expression.Set(expression.Name(k), expression.Value(v))
			first = false
		} else {
			upd = upd.Set(expression.Name(k), expression.Value(v))
		}
	}
	expr, err := expression.NewBuilder().WithUpdate(upd).Build()
	if err != nil {
		return err
	}
	_, err = s.DB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &s.Table,
		Key:                       map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: pk}},
		UpdateExpression:          expr.Update(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
	})
	return err
}

// DeleteItem removes a generic item.
func (s *Store) DeleteItem(ctx context.Context, pk string) error {
	_, err := s.DB.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &s.Table,
		Key:       map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: pk}},
	})
	return err
}

// AddNumber atomically adds delta to a numeric attribute and returns the new value.
func (s *Store) AddNumber(ctx context.Context, pk, attr string, delta float64, ttl time.Duration) (float64, error) {
	upd := expression.Add(expression.Name(attr), expression.Value(delta))
	if ttl > 0 {
		upd = upd.Set(expression.Name("ttl"), expression.Value(time.Now().Add(ttl).Unix()))
	}
	expr, err := expression.NewBuilder().WithUpdate(upd).Build()
	if err != nil {
		return 0, err
	}
	out, err := s.DB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName:                 &s.Table,
		Key:                       map[string]ddbtypes.AttributeValue{"pk": &ddbtypes.AttributeValueMemberS{Value: pk}},
		UpdateExpression:          expr.Update(),
		ExpressionAttributeNames:  expr.Names(),
		ExpressionAttributeValues: expr.Values(),
		ReturnValues:              ddbtypes.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, err
	}
	var v struct {
		N float64 `dynamodbav:"n"`
	}
	if n, ok := out.Attributes[attr].(*ddbtypes.AttributeValueMemberN); ok {
		f, _ := strconv.ParseFloat(n.Value, 64)
		v.N = f
	}
	return v.N, nil
}

// ItemsInColl lists generic items of a collection.
func (s *Store) ItemsInColl(ctx context.Context, coll string) ([]Item, error) {
	var out []Item
	err := s.queryColl(ctx, coll, 0, func(m map[string]ddbtypes.AttributeValue) error {
		it := Item{}
		if err := attributevalue.UnmarshalMap(m, &it); err != nil {
			return err
		}
		out = append(out, it)
		return nil
	})
	return out, err
}

func isConditionFailed(err error) bool {
	var ccf *ddbtypes.ConditionalCheckFailedException
	return errors.As(err, &ccf)
}

// Str returns a string field of a generic item.
func (it Item) Str(k string) string {
	if v, ok := it[k]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// Int returns an integer field of a generic item.
func (it Item) Int(k string) int64 {
	switch v := it[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// Float returns a numeric field of a generic item.
func (it Item) Float(k string) float64 {
	switch v := it[k].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case int:
		return float64(v)
	}
	return 0
}
