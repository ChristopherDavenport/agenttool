package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/openresponses"
)

// output writes what the model would see to stdout. Text is printed as
// it is. A part a shell cannot print, an image or a file carried as
// data, is written to a file under dir and its path printed in its
// place; one carried by URL prints the URL. Anything else is printed as
// its JSON.
type output struct {
	w       io.Writer
	dir     string // where files go; made on first use when empty
	program string
	files   int
}

func (o *output) write(out openresponses.FunctionCallOutputData) error {
	if out.Parts == nil {
		return o.text(out.Text)
	}
	for _, part := range out.Parts {
		var err error
		switch p := part.(type) {
		case *openresponses.Text:
			err = o.text(p.Text)
		case *openresponses.InputText:
			err = o.text(p.Text)
		case *openresponses.OutputText:
			err = o.text(p.Text)
		case *openresponses.InputImage:
			err = o.image(p)
		case *openresponses.InputFile:
			err = o.file(p)
		default:
			data, merr := json.Marshal(part)
			if merr != nil {
				return merr
			}
			err = o.text(string(data))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// text prints s, ending it with a newline when it has none, so that the
// next part or the shell's prompt starts on a line of its own.
func (o *output) text(s string) error {
	if s == "" {
		return nil
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	_, err := io.WriteString(o.w, s)
	return err
}

func (o *output) image(p *openresponses.InputImage) error {
	if typ, data, ok := parseDataURL(p.ImageURL); ok {
		return o.save("", typ, data)
	}
	if p.ImageURL != "" {
		return o.text(p.ImageURL)
	}
	return o.text("file_id: " + p.FileID)
}

func (o *output) file(p *openresponses.InputFile) error {
	if p.FileData != "" {
		if typ, data, ok := parseDataURL(p.FileData); ok {
			return o.save(p.Filename, typ, data)
		}
		if data, err := base64.StdEncoding.DecodeString(p.FileData); err == nil {
			return o.save(p.Filename, "", data)
		}
		return o.text(p.FileData)
	}
	if p.FileURL != "" {
		return o.text(p.FileURL)
	}
	if p.FileID != "" {
		return o.text("file_id: " + p.FileID)
	}
	return o.text(p.Filename)
}

// save writes data to a new file and prints its path. The file keeps
// the base of name when the tool gave one, and is otherwise numbered
// with an extension for its media type.
func (o *output) save(name, typ string, data []byte) error {
	if o.dir == "" {
		dir, err := os.MkdirTemp("", o.program+"-")
		if err != nil {
			return err
		}
		o.dir = dir
	} else if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return err
	}
	o.files++
	base := filepath.Base(name)
	if name == "" || base == "." || base == ".." || base == string(filepath.Separator) {
		base = fmt.Sprintf("output-%d%s", o.files, extension(typ))
	} else if o.files > 1 {
		base = fmt.Sprintf("%d-%s", o.files, base)
	}
	path := filepath.Join(o.dir, base)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return o.text(path)
}

// extension is the file extension for a media type: the common image
// types by name, since the system table lists several for some of them
// in no useful order, then the system's first, then none.
func extension(typ string) string {
	switch typ {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	case "", "application/octet-stream":
		return ".bin"
	}
	if exts, err := mime.ExtensionsByType(typ); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// parseDataURL splits a base64 data URL into its media type and bytes.
func parseDataURL(url string) (string, []byte, bool) {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return "", nil, false
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, false
	}
	typ, b64 := strings.CutSuffix(meta, ";base64")
	if !b64 {
		return "", nil, false
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, false
	}
	return typ, data, true
}
