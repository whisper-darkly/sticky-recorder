package recorder

import (
	"bytes"
	"text/template"
	"time"
)

// Timestamp holds the broken-out date/time fields for a single point in time.
type Timestamp struct {
	Year   string // 4-digit year
	Month  string // 2-digit month (01-12)
	Day    string // 2-digit day (01-31)
	Hour   string // 2-digit hour, 24h (00-23)
	Minute string // 2-digit minute (00-59)
	Second string // 2-digit second (00-59)
	Unix   int64  // Unix epoch seconds
}

// NewTimestamp creates a Timestamp from a time.Time.
func NewTimestamp(t time.Time) Timestamp {
	return Timestamp{
		Year:   t.Format("2006"),
		Month:  t.Format("01"),
		Day:    t.Format("02"),
		Hour:   t.Format("15"),
		Minute: t.Format("04"),
		Second: t.Format("05"),
		Unix:   t.Unix(),
	}
}

// TemplateData holds all variables available in output/log path templates.
//
// Usage examples:
//
//	{{.Source}}_{{.Session.Year}}-{{.Session.Month}}-{{.Session.Day}}
//	{{.Recording.Hour}}-{{.Recording.Minute}}-{{.Recording.Second}}
//	{{if .Segment.Count}}_{{.Segment.Count}}{{end}}
type TemplateData struct {
	Source string // Channel/source name
	Driver string // Driver name

	Session   Timestamp // When the session started (first recording of this invocation)
	Recording struct {
		Timestamp        // When the current recording started
		Count     int    // Recording number within session (0-indexed)
	}
	Segment struct {
		Timestamp        // When the current segment started
		Count     int    // Segment number within recording (0-indexed)
	}
}

// NewTemplateData creates fully-populated template data.
func NewTemplateData(source, driverName string, sessionStart, recordingStart, segmentStart time.Time, recordingNum, segmentNum int) *TemplateData {
	td := &TemplateData{
		Source:  source,
		Driver:  driverName,
		Session: NewTimestamp(sessionStart),
	}
	td.Recording.Timestamp = NewTimestamp(recordingStart)
	td.Recording.Count = recordingNum
	td.Segment.Timestamp = NewTimestamp(segmentStart)
	td.Segment.Count = segmentNum
	return td
}

// RenderTemplate evaluates a Go text/template string with the given data.
func RenderTemplate(pattern string, data *TemplateData) (string, error) {
	tpl, err := template.New("path").Parse(pattern)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
