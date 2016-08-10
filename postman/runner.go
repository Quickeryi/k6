package postman

import (
	"bytes"
	"encoding/json"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/loadimpact/speedboat/lib"
	"github.com/loadimpact/speedboat/stats"
	"golang.org/x/net/context"
	"io"
	"io/ioutil"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	mRequests = stats.Stat{Name: "requests", Type: stats.HistogramType, Intent: stats.TimeIntent}
	mErrors   = stats.Stat{Name: "errors", Type: stats.CounterType}
)

type ErrorWithLineNumber struct {
	Wrapped error
	Line    int
}

func (e ErrorWithLineNumber) Error() string {
	return fmt.Sprintf("%s (line %d)", e.Wrapped.Error(), e.Line)
}

type Runner struct {
	Collection Collection
}

type VU struct {
	Runner    *Runner
	Client    http.Client
	Collector *stats.Collector
}

func New(source []byte) (*Runner, error) {
	var collection Collection
	if err := json.Unmarshal(source, &collection); err != nil {
		switch e := err.(type) {
		case *json.SyntaxError:
			src := string(source)
			line := strings.Count(src[:e.Offset], "\n") + 1
			return nil, ErrorWithLineNumber{Wrapped: e, Line: line}
		case *json.UnmarshalTypeError:
			src := string(source)
			line := strings.Count(src[:e.Offset], "\n") + 1
			return nil, ErrorWithLineNumber{Wrapped: e, Line: line}
		}
		return nil, err
	}

	return &Runner{
		Collection: collection,
	}, nil
}

func (r *Runner) NewVU() (lib.VU, error) {
	return &VU{
		Runner: r,
		Client: http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: math.MaxInt32,
			},
		},
		Collector: stats.NewCollector(),
	}, nil
}

func (u *VU) Reconfigure(id int64) error {
	return nil
}

func (u *VU) RunOnce(ctx context.Context) error {
	for _, item := range u.Runner.Collection.Item {
		if err := u.runItem(item, u.Runner.Collection.Auth); err != nil {
			return err
		}
	}

	return nil
}

func (u *VU) runItem(i Item, a Auth) error {
	if i.Auth.Type != "" {
		a = i.Auth
	}

	if i.Request.URL != "" {
		var buffer *bytes.Buffer
		switch i.Request.Body.Mode {
		case "raw":
			buffer = bytes.NewBufferString(i.Request.Body.Raw)
		case "formdata":
			buffer = &bytes.Buffer{}
			w := multipart.NewWriter(buffer)
			for _, field := range i.Request.Body.FormData {
				if !field.Enabled {
					continue
				}

				if err := w.WriteField(field.Key, field.Value); err != nil {
					return err
				}
			}
		case "urlencoded":
			v := make(url.Values)
			for _, field := range i.Request.Body.URLEncoded {
				if !field.Enabled {
					continue
				}
				v[field.Key] = append(v[field.Key], field.Value)
			}
			buffer = bytes.NewBufferString(v.Encode())
		}

		req, err := http.NewRequest(i.Request.Method, i.Request.URL, buffer)
		if err != nil {
			return err
		}

		startTime := time.Now()
		res, err := u.Client.Do(req)
		duration := time.Since(startTime)

		status := 0
		if err == nil {
			status = res.StatusCode
			io.Copy(ioutil.Discard, res.Body)
			res.Body.Close()
		}

		tags := stats.Tags{"method": i.Request.Method, "url": i.Request.URL, "status": status}
		u.Collector.Add(stats.Sample{
			Stat:   &mRequests,
			Tags:   tags,
			Values: stats.Values{"duration": float64(duration)},
		})

		if err != nil {
			log.WithError(err).Error("Request error")
			u.Collector.Add(stats.Sample{
				Stat:   &mErrors,
				Tags:   tags,
				Values: stats.Value(1),
			})
			return err
		}
	}

	for _, item := range i.Item {
		if err := u.runItem(item, a); err != nil {
			return err
		}
	}

	return nil
}