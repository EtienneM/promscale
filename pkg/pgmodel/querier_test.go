package pgmodel

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/prometheus/prometheus/prompb"
)

type mockQuerier struct {
	tts               []*prompb.TimeSeries
	err               error
	healthCheckCalled bool
}

func (q *mockQuerier) Query(query *prompb.Query) ([]*prompb.TimeSeries, error) {
	return q.tts, q.err
}

func (q *mockQuerier) HealthCheck() error {
	q.healthCheckCalled = true
	return nil
}

func TestDBReaderRead(t *testing.T) {
	testCases := []struct {
		name string
		req  *prompb.ReadRequest
		tts  []*prompb.TimeSeries
		err  error
	}{
		{
			name: "No request",
		},
		{
			name: "No queries",
			req: &prompb.ReadRequest{
				Queries: []*prompb.Query{},
			},
		},
		{
			name: "Query error",
			req: &prompb.ReadRequest{
				Queries: []*prompb.Query{
					{StartTimestampMs: 1},
				},
			},
			err: fmt.Errorf("some error"),
		},
		{
			name: "Simple query, no results",
			req: &prompb.ReadRequest{
				Queries: []*prompb.Query{
					{StartTimestampMs: 1},
				},
			},
		},
		{
			name: "Simple query",
			req: &prompb.ReadRequest{
				Queries: []*prompb.Query{
					{StartTimestampMs: 1},
				},
			},
			tts: []*prompb.TimeSeries{
				{
					Samples: []prompb.Sample{
						{
							Timestamp: 1,
							Value:     2,
						},
					},
				},
			},
		},
		{
			name: "Multiple queries",
			req: &prompb.ReadRequest{
				Queries: []*prompb.Query{
					{StartTimestampMs: 1},
					{StartTimestampMs: 1},
					{StartTimestampMs: 1},
					{StartTimestampMs: 1},
				},
			},
			tts: []*prompb.TimeSeries{
				{
					Samples: []prompb.Sample{
						{
							Timestamp: 1,
							Value:     2,
						},
					},
				},
			},
		},
	}

	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			mq := &mockQuerier{
				tts: c.tts,
				err: c.err,
			}

			r := DBReader{mq}

			res, err := r.Read(c.req)

			if err != nil {
				if c.err == nil || err != c.err {
					t.Errorf("unexpected error:\ngot\n%s\nwanted\n%s\n", err, c.err)
				}
				return
			}

			if c.req == nil {
				if res != nil {
					t.Errorf("unexpected result:\ngot\n%v\nwanted\n%v", res, nil)
				}
				return
			}

			expRes := &prompb.ReadResponse{
				Results: make([]*prompb.QueryResult, len(c.req.Queries)),
			}

			for i := range c.req.Queries {
				expRes.Results[i] = &prompb.QueryResult{
					Timeseries: c.tts,
				}
			}

			if !reflect.DeepEqual(res, expRes) {
				t.Errorf("unexpected result:\ngot\n%v\nwanted\n%v", res, expRes)
			}

		})
	}

}

func TestHealthCheck(t *testing.T) {
	mq := &mockQuerier{}

	r := DBReader{mq}

	r.HealthCheck()

	if !mq.healthCheckCalled {
		t.Fatal("health check method not called when expected")
	}
}
