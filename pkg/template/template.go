package template

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/Ilyes512/boilr/pkg/boilr"
	"github.com/Ilyes512/boilr/pkg/prompt"
	"github.com/Ilyes512/boilr/pkg/util/osutil"
	"github.com/Ilyes512/boilr/pkg/util/stringutil"
	"github.com/Ilyes512/boilr/pkg/util/tlog"
	"github.com/Masterminds/sprig"
	"github.com/ryanuber/go-glob"
)

// Interface is contains the behavior of boilr templates.
type Interface interface {
	// Executes the template on the given target directory path.
	Execute(string) error

	// If used, the template will execute using default values.
	UseDefaultValues()

	// Returns the metadata of the template.
	Info() Metadata
}

func (t dirTemplate) Info() Metadata {
	return t.Metadata
}

// Get retrieves the template from a path.
func Get(path string) (Interface, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	// TODO make context optional
	ctxt, err := func(fname string) (map[string]interface{}, error) {
		f, err := os.Open(fname)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}

			return nil, err
		}
		defer f.Close()

		buf, err := ioutil.ReadAll(f)
		if err != nil {
			return nil, err
		}

		var metadata map[string]interface{}
		if err := json.Unmarshal(buf, &metadata); err != nil {
			return nil, err
		}

		return metadata, nil
	}(filepath.Join(absPath, boilr.ContextFileName))
	if err != nil {
		return nil, err
	}

	metadataExists, err := osutil.FileExists(filepath.Join(absPath, boilr.TemplateMetadataName))
	if err != nil {
		return nil, err
	}

	md, err := func() (Metadata, error) {
		if !metadataExists {
			return Metadata{}, nil
		}

		b, err := ioutil.ReadFile(filepath.Join(absPath, boilr.TemplateMetadataName))
		if err != nil {
			return Metadata{}, err
		}

		var m Metadata
		if err := json.Unmarshal(b, &m); err != nil {
			return Metadata{}, err
		}

		return m, nil
	}()

	return &dirTemplate{
		Context:  ctxt,
		FuncMap:  FuncMap,
		Path:     filepath.Join(absPath, boilr.TemplateDirName),
		Metadata: md,
	}, err
}

type dirTemplate struct {
	Path     string
	Context  map[string]interface{}
	FuncMap  template.FuncMap
	Metadata Metadata

	alignment         string
	ShouldUseDefaults bool
}

func (t *dirTemplate) UseDefaultValues() {
	t.ShouldUseDefaults = true
}

func (t *dirTemplate) BindPrompts() {
	for parentKey := range t.Context {
		if t.ShouldUseDefaults {
			handleBindDefaults(t, parentKey)
		} else {
			handleBindPrompts(t, parentKey)
		}
	}
}

// Execute fills the template with the project metadata.
func (t *dirTemplate) Execute(dirPrefix string) error {
	t.BindPrompts()

	isOnlyWhitespace := func(buf []byte) bool {
		wsre := regexp.MustCompile(`\S`)

		return !wsre.Match(buf)
	}

	// TODO create io.ReadWriter from string
	// TODO refactor name manipulation
	return filepath.Walk(t.Path, func(filename string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if ignoreCopyFile(filepath.Base(filename)) {
			return nil
		}

		// Path relative to the root of the template directory
		oldName, err := filepath.Rel(t.Path, filename)
		if err != nil {
			return err
		}

		buf := stringutil.NewString("")

		// TODO translate errors into meaningful ones
		fnameTmpl := template.Must(template.
			New("file name template").
			Delims("[[", "]]").
			Option(Options...).
			Funcs(sprig.TxtFuncMap()).
			Funcs(FuncMap).
			Parse(oldName))

		if err := fnameTmpl.Execute(buf, nil); err != nil {
			return err
		}

		newName := buf.String()

		splitPath := strings.Split(newName, string(filepath.Separator))
		for _, s := range splitPath {
			if strings.TrimSpace(s) == "" {
				tlog.Debug(fmt.Sprintf("skip creating a file or directory with an empty name %q -> %q", oldName, newName))
				return nil
			}
		}

		target := filepath.Join(dirPrefix, newName)

		if info.IsDir() {
			if err := os.Mkdir(target, 0755); err != nil {
				if !os.IsExist(err) {
					return err
				}
			}
		} else {
			fi, err := os.Lstat(filename)
			if err != nil {
				return err
			}

			// Delete target file if it exists
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}

			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode())
			if err != nil {
				return err
			}
			defer f.Close()

			// Open original file and check if a binary file
			origF, err := os.Open(filename)
			if err != nil {
				return err
			}
			defer origF.Close()

			isBin, err := isBinary(origF)
			if err != nil {
				return err
			}
			if isBin {
				_, err = origF.Seek(io.SeekStart, 0)
				if err != nil {
					return err
				}

				_, err = io.Copy(f, origF)
				return err
			}

			defer func(fname string) {
				contents, err := ioutil.ReadFile(fname)
				if err != nil {
					tlog.Debug(fmt.Sprintf("couldn't read the contents of file %q, got error %q", fname, err))
					return
				}

				if isOnlyWhitespace(contents) {
					os.Remove(fname)
					return
				}
			}(f.Name())

			contentsTmpl := template.Must(template.
				New("file contents template").
				Delims("[[", "]]").
				Option(Options...).
				Funcs(sprig.TxtFuncMap()).
				Funcs(FuncMap).
				ParseFiles(filename))

			fileTemplateName := filepath.Base(filename)

			if err := contentsTmpl.ExecuteTemplate(f, fileTemplateName, nil); err != nil {
				return err
			}

			if !t.ShouldUseDefaults {
				tlog.Success(fmt.Sprintf("Created %s", newName))
			}
		}

		return nil
	})
}

func ignoreCopyFile(filename string) bool {
	for _, pattern := range boilr.Configuration.IgnoreCopyFiles {
		if glob.Glob(pattern, filename) {
			return true
		}
	}

	return false
}

func handleBindDefaults(t *dirTemplate, parentKey string) {
	if childMap, ok := t.Context[parentKey].(map[string]interface{}); ok {
		if len(childMap) > 0 {
			t.FuncMap[parentKey] = func() bool { return false }
		}

		for childKey := range childMap {
			t.FuncMap[childKey] = func(val interface{}) func() interface{} {
				return func() interface{} {
					switch val := val.(type) {
					// First is the default value if it's a slice
					case []interface{}:
						return val[0]
					}

					return val
				}
			}(childMap[childKey])
		}
	} else {
		t.FuncMap[parentKey] = func(val interface{}) func() interface{} {
			return func() interface{} {
				switch val := val.(type) {
				// First is the default value if it's a slice
				case []interface{}:
					return val[0]
				}

				return val
			}
		}(t.Context[parentKey])
	}
}

func handleBindPrompts(t *dirTemplate, parentKey string) {
	if childMap, ok := t.Context[parentKey].(map[string]interface{}); ok {
		advancedMode := prompt.New(parentKey, false)

		if len(childMap) > 0 {
			t.FuncMap[parentKey] = func(a func() interface{}) func() interface{} {
				return func() interface{} {
					return advancedMode()
				}
			}(advancedMode)
		}

		for childKey := range childMap {
			childPrompt := prompt.New(childKey, childMap[childKey])

			t.FuncMap[childKey] = func(val interface{}, p func() interface{}) func() interface{} {
				return func() interface{} {
					if isAdvanced := advancedMode().(bool); isAdvanced {
						return p()
					}

					return val
				}
			}(childMap[childKey], childPrompt)
		}
	} else {
		t.FuncMap[parentKey] = prompt.New(parentKey, t.Context[parentKey])
	}
}
