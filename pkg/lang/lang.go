package lang

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/rs/zerolog"
	"github.com/spf13/afero"
	"github.com/walteh/terrors"
	"github.com/walteh/yaml"
)

func ProccessBulk(ctx context.Context, fs afero.Fs, files []string) (map[string]*FileBlockEvaluation, hcl.Diagnostics, error) {

	fles := make(map[string][]byte)

	for _, file := range files {

		opn, err := afero.ReadFile(fs, file)
		if err != nil {
			return nil, nil, err
		}

		fles[file] = opn

	}

	zerolog.Ctx(ctx).Debug().Strs("files", files).Msg("processing files")

	env, err := LoadGlobalEnvVars(fs, nil)
	if err != nil {
		return nil, nil, err
	}

	_, full, bb, diags, err := NewContextFromFiles(ctx, fles, env)
	if err != nil || diags.HasErrors() {
		return nil, diags, err
	}

	out, diags, err := NewGenBlockEvaluation(ctx, full, bb)
	if err != nil || diags.HasErrors() {
		return nil, diags, err
	}

	return out, diags, nil
}

func Process(ctx context.Context, fs afero.Fs, file string) (map[string]*FileBlockEvaluation, hcl.Diagnostics, error) {
	return ProccessBulk(ctx, fs, []string{file})
}

func (me *FileBlockEvaluation) WriteToFile(ctx context.Context, fs afero.Fs) error {
	out, erry := me.WriteToReader(ctx)
	if erry != nil {
		return terrors.Wrapf(erry, "failed to encode block %q", me.Name)
	}

	if err := fs.MkdirAll(filepath.Dir(me.Path), 0755); err != nil {
		return terrors.Wrapf(err, "failed to create directory %q", me.Path)
	}

	if err := afero.WriteReader(fs, me.Path, out); err != nil {
		return terrors.Wrapf(err, "failed to write file %q", me.Name)
	}

	return nil
}

func (me *FileBlockEvaluation) WriteToReader(ctx context.Context) (io.Reader, error) {
	out, erry := me.Encode()
	if erry != nil {
		return nil, terrors.Wrapf(erry, "failed to encode block %q", me.Name)
	}

	return bytes.NewReader(out), nil
}

func (me *FileBlockEvaluation) Encode() ([]byte, error) {

	arr := strings.Split(me.Path, ".")
	if len(arr) < 2 {
		return nil, terrors.Errorf("invalid file name [%s] - missing extension", me.Name)
	}
	vers := "v0.0.0-unknown"
	v, ok := debug.ReadBuildInfo()
	if ok {
		vers = v.Main.Version
	}

	vers = strings.TrimSpace(vers)
	if vers == "" {
		vers = "v0.0.0-unknown"
	}

	header := fmt.Sprintf(`# code generated by retab %s. DO NOT EDIT.
# join the fight against yaml @ github.com/walteh/retab

# source: %q

`, vers, me.Source)

	switch arr[len(arr)-1] {
	case "jsonc", "code-workspace":

		if me.Schema != "" {
			// # yaml-language-server: $schema=https://goreleaser.com/static/schema.json
			header += fmt.Sprintf("# yaml-language-server: $schema=%s\n\n", me.Schema)
			// header +=
		}

		buf := bytes.NewBuffer(nil)
		enc := json.NewEncoder(buf)
		enc.SetIndent("", "\t")

		err := enc.Encode(me.OrderedOutput)
		if err != nil {
			return nil, err
		}

		return []byte(strings.ReplaceAll(header, "#", "//") + buf.String()), nil
	case "json":

		return json.MarshalIndent(me.OrderedOutput, "", "\t")
	case "yaml", "yml":
		if me.Schema != "" {
			// # yaml-language-server: $schema=https://goreleaser.com/static/schema.json
			header += fmt.Sprintf("# yaml-language-server: $schema=%s\n", me.Schema)
		}
		buf := bytes.NewBuffer(nil)
		enc := yaml.NewEncoder(buf)
		// enc.SetIndent(4)
		defer enc.Close()

		err := enc.Encode(me.OrderedOutput)
		if err != nil {
			return nil, err
		}

		strWithTabsRemovedFromHeredoc := strings.ReplaceAll(buf.String(), "\t", "")

		return []byte(header + strWithTabsRemovedFromHeredoc), nil

	default:
		return nil, terrors.Errorf("unknown file extension [%s] in %s", arr[len(arr)-1], me.Name)
	}
}
