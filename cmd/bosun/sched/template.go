package sched

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bosun.org/cmd/bosun/conf"
	"bosun.org/cmd/bosun/expr"
	"bosun.org/models"
	"bosun.org/opentsdb"
	"bosun.org/slog"
	"github.com/aymerick/douceur/inliner"
	"github.com/jmoiron/jsonq"
)

type Context struct {
	*models.IncidentState
	Alert   *conf.Alert
	IsEmail bool
	Errors  []string

	schedule    *Schedule
	runHistory  *RunHistory
	Attachments []*models.Attachment
}

func (s *Schedule) Data(rh *RunHistory, st *models.IncidentState, a *conf.Alert, isEmail bool) *Context {
	c := Context{
		IncidentState: st,
		Alert:         a,
		IsEmail:       isEmail,
		schedule:      s,
		runHistory:    rh,
	}
	return &c
}

type unknownContext struct {
	Time  time.Time
	Name  string
	Group models.AlertKeys

	schedule *Schedule
}

func (s *Schedule) unknownData(t time.Time, name string, group models.AlertKeys) *unknownContext {
	return &unknownContext{
		Time:     t,
		Group:    group,
		Name:     name,
		schedule: s,
	}
}

// Ack returns the URL to acknowledge an alert.
func (c *Context) Ack() string {
	return c.schedule.SystemConf.MakeLink("/action", &url.Values{
		"type": []string{"ack"},
		"key":  []string{c.Alert.Name + c.AlertKey.Group().String()},
	})
}

// HostView returns the URL to the host view page.
func (c *Context) HostView(host string) string {
	return c.schedule.SystemConf.MakeLink("/host", &url.Values{
		"time": []string{"1d-ago"},
		"host": []string{host},
	})
}

// Hack so template can read IncidentId off of event.
func (c *Context) Last() interface{} {
	return struct {
		models.Event
		IncidentId int64
	}{c.IncidentState.Last(), c.Id}
}

// Expr takes an expression in the form of a string, changes the tags to
// match the context of the alert, and returns a link to the expression page.
func (c *Context) Expr(v string) string {
	p := url.Values{}
	p.Add("date", c.runHistory.Start.Format(`2006-01-02`))
	p.Add("time", c.runHistory.Start.Format(`15:04:05`))
	p.Add("expr", base64.StdEncoding.EncodeToString([]byte(opentsdb.ReplaceTags(v, c.AlertKey.Group()))))
	return c.schedule.SystemConf.MakeLink("/expr", &p)
}

// GraphLink takes an expression in the form of a string, and returns a link to
// the expression page's graph tab with the time set.
func (c *Context) GraphLink(v string) string {
	p := url.Values{}
	p.Add("expr", base64.StdEncoding.EncodeToString([]byte(v)))
	p.Add("tab", "graph")
	p.Add("date", c.runHistory.Start.Format(`2006-01-02`))
	p.Add("time", c.runHistory.Start.Format(`15:04:05`))
	return c.schedule.SystemConf.MakeLink("/expr", &p)
}

func (c *Context) Rule() (string, error) {
	p := url.Values{}
	time := c.runHistory.Start
	p.Add("alert", c.Alert.Name)
	p.Add("fromDate", time.Format("2006-01-02"))
	p.Add("fromTime", time.Format("15:04"))
	p.Add("template_group", c.Tags)
	return c.schedule.SystemConf.MakeLink("/config", &p), nil
}

func (c *Context) Incident() string {
	return c.schedule.SystemConf.MakeLink("/incident", &url.Values{
		"id": []string{fmt.Sprint(c.Id)},
	})
}

func (s *Schedule) ExecuteBody(rh *RunHistory, a *conf.Alert, st *models.IncidentState, isEmail bool) ([]byte, []*models.Attachment, error) {
	t := a.Template
	if t == nil || t.Body == nil {
		return nil, nil, nil
	}
	c := s.Data(rh, st, a, isEmail)
	buf := new(bytes.Buffer)
	if err := t.Body.Execute(buf, c); err != nil {
		return nil, nil, err
	}
	if inline, err := inliner.Inline(buf.String()); err == nil {
		buf = bytes.NewBufferString(inline)
	} else {
		slog.Errorln(err)
	}
	return buf.Bytes(), c.Attachments, nil
}

func (s *Schedule) ExecuteSubject(rh *RunHistory, a *conf.Alert, st *models.IncidentState, isEmail bool) ([]byte, error) {
	t := a.Template
	if t == nil || t.Subject == nil {
		return nil, nil
	}
	buf := new(bytes.Buffer)
	err := t.Subject.Execute(buf, s.Data(rh, st, a, isEmail))
	return bytes.Join(bytes.Fields(buf.Bytes()), []byte(" ")), err
}

var error_body = template.Must(template.New("body_error_template").Parse(`
	<p>There was a runtime error processing alert {{.State.AlertKey}} using the {{.Alert.Template.Name}} template. The following errors occurred:</p>
	<ul>
	{{range .Errors}}
		<li>{{.}}</li>
	{{end}}
	</ul>
	<p>Use <a href="{{.Rule}}">this link</a> to the rule page to correct this.</p>
	<h2>Generic Alert Information</h2>
	<p>Status: {{.Last.Status}}</p>
	<p>Alert: {{.State.AlertKey}}</p>
	<h3>Computations</h3>
	<table>
		<tr>
			<th style="text-align:left">Expression</th>
			<th style="text-align:left">Value</th>
		</tr>
	{{range .Computations}}
		<tr>
			<td style="text-align:left">{{.Text}}</td>
			<td style="text-align:left">{{.Value}}</td>
		</tr>
	{{end}}</table>`))

func (s *Schedule) ExecuteBadTemplate(errs []error, rh *RunHistory, a *conf.Alert, st *models.IncidentState) (subject, body []byte, err error) {
	sub := fmt.Sprintf("error: template rendering error for alert %v", st.AlertKey)
	c := struct {
		Errors []error
		*Context
	}{
		Errors:  errs,
		Context: s.Data(rh, st, a, true),
	}
	buf := new(bytes.Buffer)
	error_body.Execute(buf, c)
	return []byte(sub), buf.Bytes(), nil
}

func (c *Context) evalExpr(e *expr.Expr, filter bool, series bool, autods int) (expr.ResultSlice, string, error) {
	var err error
	if filter {
		e, err = expr.New(opentsdb.ReplaceTags(e.Text, c.AlertKey.Group()), c.schedule.RuleConf.GetFuncs(c.schedule.SystemConf.EnabledBackends()))
		if err != nil {
			return nil, "", err
		}
	}
	if series && e.Root.Return() != models.TypeSeriesSet {
		return nil, "", fmt.Errorf("need a series, got %T (%v)", e, e)
	}
	providers := &expr.BosunProviders{
		Cache:     c.runHistory.Cache,
		Search:    c.schedule.Search,
		Squelched: c.schedule.RuleConf.AlertSquelched(c.Alert),
		History:   c.schedule,
	}
	res, _, err := e.Execute(c.runHistory.Backends, providers, nil, c.runHistory.Start, autods, c.Alert.UnjoinedOK)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %v", e, err)
	}
	return res.Results, e.String(), nil
}

// eval takes an expression or string (which it turns into an expression), executes it and returns the result.
// It can also takes a ResultSlice so callers can transparantly handle different inputs.
// The filter argument constrains the result to matching tags in the current context.
// The series argument asserts that the result is a time series.
func (c *Context) eval(v interface{}, filter bool, series bool, autods int) (res expr.ResultSlice, title string, err error) {
	switch v := v.(type) {
	case string:
		var e *expr.Expr
		e, err = expr.New(v, c.schedule.RuleConf.GetFuncs(c.schedule.SystemConf.EnabledBackends()))
		if err != nil {
			return nil, "", fmt.Errorf("%s: %v", v, err)
		}
		res, title, err = c.evalExpr(e, filter, series, autods)
		if err != nil {
			return
		}
	case *expr.Expr:
		res, title, err = c.evalExpr(v, filter, series, autods)
		if err != nil {
			return
		}
	case expr.ResultSlice:
		res = v
	default:
		return nil, "", fmt.Errorf("expected string, expression or resultslice, got %T (%v)", v, v)
	}
	if filter {
		res = res.Filter(c.AlertKey.Group())
	}
	if series {
		for _, k := range res {
			if k.Type() != models.TypeSeriesSet {
				return nil, "", fmt.Errorf("need a series, got %v (%v)", k.Type(), k)
			}
		}
	}
	return res, title, err
}

// Lookup returns the value for a key in the lookup table for the context's tagset.
// the returned string may be the representation of an error
func (c *Context) Lookup(table, key string) string {
	return c.LookupAll(table, key, c.AlertKey.Group())
}

func (c *Context) LookupAll(table, key string, group interface{}) string {
	var t opentsdb.TagSet
	switch v := group.(type) {
	case string:
		var err error
		t, err = opentsdb.ParseTags(v)
		if err != nil {
			c.addError(err)
			return err.Error()
		}
	case opentsdb.TagSet:
		t = v
	}
	l := c.schedule.RuleConf.GetLookup(table)
	if l == nil {
		err := fmt.Errorf("unknown lookup table %v", table)
		c.addError(err)
		return err.Error()
	}
	if v, ok := l.ToExpr().Get(key, t); ok {
		return v
	}
	err := fmt.Errorf("no entry for key %v in table %v for tagset %v", key, table, c.AlertKey.Group())
	c.addError(err)
	return err.Error()
}

func (c *Context) addError(e error) {
	c.Errors = append(c.Errors, e.Error())
}

// Eval takes a result or an expression which it evaluates to a result.
// It returns a value with tags corresponding to the context's tags.
// If no such result is found, the first result with
// nil tags is returned. If no such result is found, nil is returned.
func (c *Context) Eval(v interface{}) interface{} {
	res, _, err := c.eval(v, true, false, 0)
	if err != nil {
		c.addError(err)
		return nil
	}
	if len(res) == 0 {
		return math.NaN()
	}
	// TODO: don't choose a random result, make sure there's exactly 1
	return res[0].Value
}

// EvalAll returns the executed expression (or the given result as is).
func (c *Context) EvalAll(v interface{}) interface{} {
	res, _, err := c.eval(v, false, false, 0)
	if err != nil {
		c.addError(err)
		return nil
	}
	return res
}

func (c *Context) graph(v interface{}, unit string, filter bool) (val interface{}) {
	defer func() {
		if p := recover(); p != nil {
			err := fmt.Errorf("panic rendering graph %v", p)
			c.addError(err)
			slog.Error(err)
			val = err.Error()
		}
	}()
	res, exprText, err := c.eval(v, filter, true, 1000)
	if err != nil {
		c.addError(err)
		return err.Error()
	}
	var buf bytes.Buffer
	const width = 800
	const height = 600
	footerHTML := fmt.Sprintf(`<p><small>Query: %s<br>Time: %s</small></p>`,
		template.HTMLEscapeString(exprText),
		c.runHistory.Start.Format(time.RFC3339))
	if c.IsEmail {
		err := c.schedule.ExprPNG(nil, &buf, width, height, unit, res)
		if err != nil {
			c.addError(err)
			return err.Error()
		}
		name := fmt.Sprintf("%d.png", len(c.Attachments)+1)
		c.Attachments = append(c.Attachments, &models.Attachment{
			Data:        buf.Bytes(),
			Filename:    name,
			ContentType: "image/png",
		})
		return template.HTML(fmt.Sprintf(`<a href="%s" style="text-decoration: none"><img alt="%s" src="cid:%s" /></a>%s`,
			c.GraphLink(exprText),
			template.HTMLEscapeString(fmt.Sprint(v)),
			name,
			footerHTML,
		))
	}
	buf.WriteString(fmt.Sprintf(`<a href="%s" style="text-decoration: none">`, c.GraphLink(exprText)))
	if err := c.schedule.ExprSVG(nil, &buf, width, height, unit, res); err != nil {
		c.addError(err)
		return err.Error()
	}
	buf.WriteString(`</a>`)
	buf.WriteString(footerHTML)
	return template.HTML(buf.String())
}

// Graph returns an SVG for the given result (or expression, for which it gets the result)
// with same tags as the context's tags.
func (c *Context) Graph(v interface{}, args ...string) interface{} {
	var unit string
	if len(args) > 0 {
		unit = args[0]
	}
	return c.graph(v, unit, true)
}

// GraphAll returns an SVG for the given result (or expression, for which it gets the result).
func (c *Context) GraphAll(v interface{}, args ...string) interface{} {
	var unit string
	if len(args) > 0 {
		unit = args[0]
	}
	return c.graph(v, unit, false)
}

func (c *Context) GetMeta(metric, name string, v interface{}) interface{} {
	var t opentsdb.TagSet
	switch v := v.(type) {
	case string:
		var err error
		t, err = opentsdb.ParseTags(v)
		if err != nil {
			c.addError(err)
			return nil
		}
	case opentsdb.TagSet:
		t = v
	}
	meta, err := c.schedule.GetMetadata(metric, t)
	if err != nil {
		c.addError(err)
		return nil
	}
	if name == "" {
		return meta
	}
	for _, m := range meta {
		if m.Name == name {
			return m.Value
		}
	}
	return "metadta not found"
}

// LeftJoin takes slices of results and expressions for which it gets the slices of results.
// Then it joins the 2nd and higher slice of results onto the first slice of results.
// Joining is performed by group: a group that includes all tags (with same values) of the first group is a match.
func (c *Context) LeftJoin(v ...interface{}) (interface{}, error) {
	if len(v) < 2 {
		// A template error is thrown here since this should be caught when defining at testing the template
		return nil, fmt.Errorf("need at least two values (each can be an expression or result slice), got %v", len(v))
	}
	// temporarily store the results in a results[M][Ni] Result matrix:
	// for M queries, tracks Ni results per each i'th query
	results := make([][]*expr.Result, len(v))
	for col, val := range v {
		queryResults, _, err := c.eval(val, false, false, 0)
		if err != nil {
			c.addError(err)
			return nil, nil
		}
		results[col] = queryResults
	}

	// perform the joining by storing all results in a joined[N0][M] Result matrix:
	// for N tagsets (based on first query results), tracks all M Results (results with matching group, from all other queries)
	joined := make([][]*expr.Result, 0)
	for row, firstQueryResult := range results[0] {
		joined = append(joined, make([]*expr.Result, len(v)))
		joined[row][0] = firstQueryResult
		// join results of 2nd to M queries
		for col, queryResults := range results[1:] {
			for _, laterQueryResult := range queryResults {
				if firstQueryResult.Group.Subset(laterQueryResult.Group) {
					joined[row][col+1] = laterQueryResult
					break
				}
				// Fill emtpy cells with NaN Value, so calling .Value is not a nil pointer dereference
				joined[row][col+1] = &expr.Result{Value: expr.Number(math.NaN())}
			}
		}
	}
	return joined, nil
}

func (c *Context) HTTPGet(url string) string {
	resp, err := http.Get(url)
	if err != nil {
		c.addError(err)
		return err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Drain up to 512 bytes and close the body to let the Transport reuse the connection
		io.CopyN(ioutil.Discard, resp.Body, 512)
		err := fmt.Errorf("%v: returned %v", url, resp.Status)
		c.addError(err)
		return err.Error()
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		c.addError(err)
		return err.Error()
	}
	return string(body)
}

func (c *Context) HTTPGetJSON(url string) *jsonq.JsonQuery {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		c.addError(err)
		return nil
	}
	req.Header.Set("Accept", "application/json")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		c.addError(err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		c.addError(fmt.Errorf("%v: returned %v", url, resp.Status))
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		c.addError(err)
		return nil
	}
	data := make(map[string]interface{})
	err = json.Unmarshal(body, &data)
	if err != nil {
		c.addError(err)
		return nil
	}
	return jsonq.NewQuery(data)
}

func (c *Context) HTTPPost(url, bodyType, data string) string {
	resp, err := http.Post(url, bodyType, bytes.NewBufferString(data))
	if err != nil {
		c.addError(err)
		return err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Drain up to 512 bytes and close the body to let the Transport reuse the connection
		io.CopyN(ioutil.Discard, resp.Body, 512)
		return fmt.Sprintf("%v: returned %v", url, resp.Status)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		c.addError(err)
		return err.Error()
	}
	return string(body)
}

func (c *Context) LSQuery(index_root, filter, sduration, eduration string, size int) interface{} {
	var ks []string
	for k, v := range c.AlertKey.Group() {
		ks = append(ks, k+":"+v)
	}
	return c.LSQueryAll(index_root, strings.Join(ks, ","), filter, sduration, eduration, size)
}

func (c *Context) LSQueryAll(index_root, keystring, filter, sduration, eduration string, size int) interface{} {
	req, err := expr.LSBaseQuery(c.runHistory.Start, index_root, keystring, filter, sduration, eduration, size)
	if err != nil {
		c.addError(err)
		return nil
	}
	results, err := c.runHistory.Backends.LogstashHosts.Query(req)
	if err != nil {
		c.addError(err)
		return nil
	}
	r := make([]interface{}, len(results.Hits.Hits))
	for i, h := range results.Hits.Hits {
		var err error
		err = json.Unmarshal(*h.Source, &r[i])
		if err != nil {
			c.addError(err)
			return nil
		}
	}
	return r
}

func (c *Context) ESQuery(indexRoot expr.ESIndexer, filter expr.ESQuery, sduration, eduration string, size int) interface{} {
	newFilter := expr.ScopeES(c.Group(), filter.Query)
	req, err := expr.ESBaseQuery(c.runHistory.Start, indexRoot, newFilter, sduration, eduration, size)
	if err != nil {
		c.addError(err)
		return nil
	}
	results, err := c.runHistory.Backends.ElasticHosts.Query(req)
	if err != nil {
		c.addError(err)
		return nil
	}
	r := make([]interface{}, len(results.Hits.Hits))
	for i, h := range results.Hits.Hits {
		var err error
		err = json.Unmarshal(*h.Source, &r[i])
		if err != nil {
			c.addError(err)
			return nil
		}
	}
	return r
}

func (c *Context) ESQueryAll(indexRoot expr.ESIndexer, filter expr.ESQuery, sduration, eduration string, size int) interface{} {
	req, err := expr.ESBaseQuery(c.runHistory.Start, indexRoot, filter.Query, sduration, eduration, size)
	if err != nil {
		c.addError(err)
		return nil
	}
	results, err := c.runHistory.Backends.ElasticHosts.Query(req)
	if err != nil {
		c.addError(err)
		return nil
	}
	r := make([]interface{}, len(results.Hits.Hits))
	for i, h := range results.Hits.Hits {
		var err error
		err = json.Unmarshal(*h.Source, &r[i])
		if err != nil {
			c.addError(err)
			return nil
		}
	}
	return r
}

type actionNotificationContext struct {
	States     []*models.IncidentState
	User       string
	Message    string
	ActionType models.ActionType

	schedule *Schedule
}

func (a actionNotificationContext) IncidentLink(i int64) string {
	return a.schedule.SystemConf.MakeLink("/incident", &url.Values{
		"id": []string{fmt.Sprint(i)},
	})
}
