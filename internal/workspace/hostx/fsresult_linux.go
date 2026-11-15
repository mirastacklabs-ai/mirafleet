//go:build linux

package hostx

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/mirastacklabs-ai/mirafleet/internal/transport"
	"github.com/mirastacklabs-ai/mirafleet/internal/workspace/fsops"
)

// ConvertFS turns the raw result of Files.Do into the transport result plus the image bytes that
// follow it on the wire (Length is their count). Same mapping as the fake executor.
func ConvertFS(raw any) (transport.FSResult, []byte, error) {
	switch r := raw.(type) {
	case *fsops.ReadResult:
		res := transport.FSResult{Kind: string(r.Kind), Content: r.Text, Size: r.Size, TotalLines: r.TotalLines,
			Truncated: r.More, MIME: r.MIMEType, SHA256: r.SHA256}
		if r.EndLine >= r.StartLine && r.StartLine > 0 {
			res.Lines = r.EndLine - r.StartLine + 1
		}
		if len(r.Images) > 0 {
			img, err := base64.StdEncoding.DecodeString(r.Images[0].Data)
			if err != nil {
				return res, nil, err
			}
			res.Kind, res.MIME, res.Length = transport.KindImage, r.Images[0].MIME, int64(len(img))
			return res, img, nil
		}
		return res, nil, nil
	case *fsops.WriteResult:
		return transport.FSResult{Kind: transport.KindNone, NewFile: r.Created, Size: r.Size, SHA256: r.SHA256,
			WrittenBytes: r.Size}, nil, nil
	case *fsops.EditResult:
		return transport.FSResult{Kind: transport.KindNone, Count: r.Replacements, Size: r.Size, SHA256: r.SHA256,
			NewFile: r.Created, Content: r.Snippet}, nil, nil
	case *fsops.NotebookEditResult:
		return transport.FSResult{Kind: transport.KindNone, Count: r.Cells, Size: r.Size, SHA256: r.SHA256,
			Content: r.CellID}, nil, nil
	case fsops.GrepResult:
		var lines []string
		switch {
		case len(r.Lines) > 0:
			lines = r.Lines
		case len(r.Files) > 0:
			lines = r.Files
		default:
			for _, c := range r.Counts {
				lines = append(lines, fmt.Sprintf("%s:%d", c.Path, c.Count))
			}
		}
		return transport.FSResult{Kind: transport.KindText, Content: strings.Join(lines, "\n"), Count: len(lines),
			Truncated: r.Truncated}, nil, nil
	case fsops.GlobResult:
		res := transport.FSResult{Kind: transport.KindEntries, Count: len(r.Paths), Truncated: r.Truncated}
		for _, p := range r.Paths {
			res.Entries = append(res.Entries, transport.FSEntry{Path: p})
		}
		return res, nil, nil
	case *fsops.LSResult:
		return transport.FSResult{Kind: transport.KindText, Content: r.Text, Count: r.Count, Truncated: r.Truncated}, nil, nil
	case *fsops.Info:
		return transport.FSResult{Kind: transport.KindEntries, Count: 1, Entries: []transport.FSEntry{{Path: r.Path,
			Size: r.Size, Mode: uint32(r.Mode.Perm()), MTimeMs: r.ModTime.UnixMilli(), Dir: r.Type == "dir"}}}, nil, nil
	}
	return transport.FSResult{}, nil, fmt.Errorf("hostx: unexpected fs result %T", raw)
}
