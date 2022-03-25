package hermit_test

import (
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cashapp/hermit"
	"github.com/cashapp/hermit/envars"
	"github.com/cashapp/hermit/hermittest"
	"github.com/cashapp/hermit/manifest"
	"github.com/cashapp/hermit/manifest/manifesttest"
)

// Test that when installing a package that has binaries conflicting
// with an existing package, we get an error
func TestConflictingBinariesError(t *testing.T) {
	fixture := hermittest.NewEnvTestFixture(t, nil)
	defer fixture.Clean()

	pkg1 := manifesttest.NewPkgBuilder(fixture.RootDir()).
		WithSource("archive/testdata/archive.tar.gz").
		Result()

	pkg2 := manifesttest.NewPkgBuilder(fixture.RootDir()).
		WithSource("archive/testdata/archive.tar.gz").
		WithName("test2").
		WithVersion("1").
		Result()

	_, err := fixture.Env.Install(fixture.P, pkg1)
	require.NoError(t, err)

	_, err = fixture.Env.Install(fixture.P, pkg2)
	require.Errorf(t, err, "test2-1 can not be installed, the following binaries already exist: darwin_exe, linux_exe")
}

// Test that the update timestamp and etag are written to the DB correctly when
// installing a package with an update interval
func TestUpdateTimestampOnInstall(t *testing.T) {
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("ETag", "testtag")
		dat, _ := ioutil.ReadFile("archive/testdata/archive.tar.gz")
		_, err := w.Write(dat)
		require.NoError(t, err)
		calls++
	})
	fixture := hermittest.NewEnvTestFixture(t, handler)
	defer fixture.Clean()

	pkg := manifesttest.NewPkgBuilder(fixture.RootDir()).
		WithName("test").
		WithChannel("stable").
		WithUpdateInterval(1 * time.Hour).
		WithSource(fixture.Server.URL).
		Result()

	_, err := fixture.Env.Install(fixture.P, pkg)
	require.NoError(t, err)

	dbPkg := fixture.GetDBPackage("test@stable")
	actual := dbPkg.UpdateCheckedAt.Unix()
	require.GreaterOrEqual(t, time.Now().Add(1*time.Minute).Unix(), actual)
	require.LessOrEqual(t, time.Now().Add(-1*time.Minute).Unix(), actual)
	require.Equal(t, "testtag", dbPkg.Etag)
	require.Equal(t, 1, calls)
}

// Tests that EnsureUpToDate only updates the package when the etag has changed
func TestEnsureUpToDate(t *testing.T) {
	etag := "first"
	data := "data"
	headCalls := 0
	getCalls := 0
	fail := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("ETag", etag)
		if fail {
			w.WriteHeader(500)
		} else if r.Method == "HEAD" {
			headCalls++
		} else if r.Method == "GET" {
			getCalls++
			tar := TestTarGz{map[string]string{"bin": data}}
			tar.Write(t, w)
		}
	})
	fixture := hermittest.NewEnvTestFixture(t, handler)
	defer fixture.Clean()
	dao := fixture.DAO()

	pkg := manifesttest.NewPkgBuilder(fixture.RootDir()).
		WithName("test").
		WithBinaries("bin").
		WithChannel("chan").
		WithUpdateInterval(1 * time.Hour).
		WithSource(fixture.Server.URL).
		Result()

	// Initial install
	_, err := fixture.Env.Install(fixture.P, pkg)
	require.NoError(t, err)
	require.Equal(t, 1, getCalls)
	require.Equal(t, 0, headCalls)

	// Update before update check is due
	err = fixture.Env.EnsureChannelIsUpToDate(fixture.P, pkg)
	require.NoError(t, err)
	require.Equal(t, 1, getCalls)
	require.Equal(t, 0, headCalls)
	file, _ := ioutil.ReadFile(filepath.Join(pkg.Dest, "bin"))
	require.Equal(t, data, string(file))

	// Update after a check is needed but etag has not changed
	pkg.UpdatedAt = time.Now().Add(-2 * time.Hour)
	err = fixture.Env.EnsureChannelIsUpToDate(fixture.P, pkg)
	require.NoError(t, err)
	require.Equal(t, 1, getCalls)
	require.Equal(t, 1, headCalls)
	file, _ = ioutil.ReadFile(filepath.Join(pkg.Dest, "bin"))
	require.Equal(t, data, string(file))

	// Update after a check is needed and the etag has changed
	pkg.UpdatedAt = time.Now().Add(-2 * time.Hour)
	etag = "changed"
	data = strings.Repeat("other", 1024)
	err = fixture.Env.EnsureChannelIsUpToDate(fixture.P, pkg)
	require.NoError(t, err)
	require.Equal(t, 2, getCalls)
	require.Equal(t, 2, headCalls)
	file, _ = ioutil.ReadFile(filepath.Join(pkg.Dest, "bin"))
	require.Equal(t, data, string(file))

	// Check that the package is still in the DB after the upgrade
	dbPkg, err := dao.GetPackage(pkg.Reference.String())
	require.NoError(t, err)
	require.NotNil(t, dbPkg)

	// Check etag retained when the connection fails
	fail = true
	pkg.UpdatedAt = time.Now().Add(-2 * time.Hour)
	err = fixture.Env.EnsureChannelIsUpToDate(fixture.P, pkg)
	require.NoError(t, err)
	dbPkg, err = dao.GetPackage(pkg.Reference.String())
	require.NoError(t, err)
	require.Equal(t, etag, dbPkg.Etag)
}

// Test that files referred in the Files map are copied correctly
func TestCopyFiles(t *testing.T) {
	dir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	f, err := os.Create(filepath.Join(dir, "from"))
	require.NoError(t, err)
	err = f.Close()
	require.NoError(t, err)

	fixture := hermittest.NewEnvTestFixture(t, nil)
	defer fixture.Clean()

	pkg := manifesttest.NewPkgBuilder(fixture.RootDir()).
		WithSource("archive/testdata/archive.tar.gz").
		WithVersion("1").
		WithFile("from", filepath.Join(dir, "to"), os.DirFS(dir)).
		Result()
	_, err = fixture.Env.Install(fixture.P, pkg)
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(dir, "to"))
	require.NoError(t, err)
}

// Test that files referred in the Files map are copied correctly
func TestCopyFilesAction(t *testing.T) {
	dir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	f, err := os.Create(filepath.Join(dir, "from"))
	require.NoError(t, err)
	err = f.Close()
	require.NoError(t, err)

	fixture := hermittest.NewEnvTestFixture(t, nil)
	defer fixture.Clean()

	pkg := manifesttest.NewPkgBuilder(fixture.RootDir()).
		WithSource("archive/testdata/archive.tar.gz").
		WithVersion("1").
		WithFS(os.DirFS(dir)).
		WithTrigger(manifest.EventUnpack, &manifest.CopyAction{
			From: "from",
			To:   filepath.Join(dir, "to"),
			Mode: 0755,
		}).
		Result()
	_, err = fixture.Env.Install(fixture.P, pkg)
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(dir, "to"))
	require.NoError(t, err)
	require.Equal(t, 0755, int(info.Mode()))
}

func TestTriggers(t *testing.T) {
	dir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	file := filepath.Join(dir, "test.sh")
	target := filepath.Join(dir, "success")

	fd, err := os.Create(file)
	require.NoError(t, err)
	_, err = fd.WriteString("#!/bin/sh\ntouch " + target)
	require.NoError(t, err)
	err = fd.Close()
	require.NoError(t, err)

	fixture := hermittest.NewEnvTestFixture(t, nil)
	defer fixture.Clean()

	pkg := manifesttest.NewPkgBuilder(fixture.RootDir()).
		WithSource("archive/testdata/archive.tar.gz").
		WithVersion("1").
		WithTrigger(manifest.EventUnpack,
			&manifest.RunAction{
				Command: "/bin/sh",
				Dir:     dir,
				Args:    []string{file},
				Env:     nil,
				Stdin:   "",
			}).
		Result()
	_, err = fixture.Env.Install(fixture.P, pkg)
	require.NoError(t, err)

	_, err = os.Stat(target)
	require.NoError(t, err)
}

func TestExpandEnvars(t *testing.T) {
	tests := []struct {
		in       []string
		ops      []string
		expected []string
	}{
		{in: []string{
			"PATH=/usr/local/bin:/usr/bin",
			"HERMIT_STATE_DIR=/tmp/cache/hermit",
			"HERMIT_ENV=/tmp/env",
		},
			ops: []string{
				"NODE_PATH=${HERMIT_STATE_DIR}/pkg/node",
				"PATH=${HERMIT_ENV}/bin:${PATH}",
				"PATH=${NODE_PATH}/bin:${PATH}",
			},
			expected: []string{
				"HERMIT_ENV=/tmp/env",
				"HERMIT_STATE_DIR=/tmp/cache/hermit",
				"NODE_PATH=/tmp/cache/hermit/pkg/node",
				"PATH=/tmp/cache/hermit/pkg/node/bin:/tmp/env/bin:/usr/local/bin:/usr/bin",
			},
		},
		{in: []string{},
			ops: []string{
				"A=${B}",
				"B=${A}",
			},
			expected: []string{},
		},
	}
	for _, test := range tests {
		ops := envars.Infer(test.ops)
		actual := envars.Parse(test.in).
			Apply("", ops).
			Combined().
			System()
		require.Equal(t, test.expected, actual)
	}
}

func TestDependencyResolution(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tar := TestTarGz{map[string]string{"bin1": "foo"}}
		tar.Write(t, w)
	})

	f := hermittest.NewEnvTestFixture(t, handler)
	f.WithManifests(map[string]string{
		"dep.hcl": `
			description = ""
			binaries = ["bin1"]
			version "1.0.0" {
			  source = "` + f.Server.URL + `"
			}
			provides = ["virtual"]
		`,
		"pkg1.hcl": `
			description = ""
			binaries = ["bin1"]
			version "1.0.0" {
			  source = "` + f.Server.URL + `"
			}
			requires = ["dep"]
			provides = ["virtual2"]
		`,
		"pkg2.hcl": `
			description = ""
			binaries = ["bin1"]
			version "1.0.0" {
			  source = "` + f.Server.URL + `"
			}
			requires = ["virtual"]
			provides = ["virtual2"]
		`,
		"pkg3.hcl": `
			description = ""
			binaries = ["bin1"]
			version "1.0.0" {
			  source = "` + f.Server.URL + `"
			}
			requires = ["not-found"]
		`,
		"pkg4.hcl": `
			description = ""
			binaries = ["bin1"]
			version "1.0.0" {
			  source = "` + f.Server.URL + `"
			}
			requires = ["virtual2"]
		`,
	})
	defer f.Clean()

	pkg, err := f.Env.Resolve(f.P, manifest.NameSelector("dep"), false)
	require.NoError(t, err)
	_, err = f.Env.Install(f.P, pkg)
	require.NoError(t, err)

	installed, err := f.Env.ListInstalledReferences()
	require.NoError(t, err)

	// Test that dependencies can be resolved based on the package name
	err = f.Env.ResolveWithDeps(f.P, installed, manifest.NameSelector("pkg1"), map[string]*manifest.Package{})
	require.NoError(t, err)

	// Test that dependencies can be resolved based on the virtual package name
	err = f.Env.ResolveWithDeps(f.P, installed, manifest.NameSelector("pkg2"), map[string]*manifest.Package{})
	require.NoError(t, err)

	// Test that missing dependencies fail
	err = f.Env.ResolveWithDeps(f.P, installed, manifest.NameSelector("pkg3"), map[string]*manifest.Package{})
	require.ErrorIs(t, err, manifest.ErrUnknownPackage)

	// Test that resolving package where requirement is fulfilled by multiple uninstalled packages fails
	err = f.Env.ResolveWithDeps(f.P, installed, manifest.NameSelector("pkg4"), map[string]*manifest.Package{})
	require.Errorf(t, err, "multiple packages satisfy the required dependency \"virtual2\", please install one of the following manually: pkg1, pkg2")
}

func TestManifestValidation(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bar" {
			w.WriteHeader(http.StatusNotFound)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	})

	f := hermittest.NewEnvTestFixture(t, handler)
	f.WithManifests(map[string]string{
		"test.hcl": `
			description = ""
			binaries = ["bin1"]
			version "1.0.0" {
		      linux { source = "` + f.Server.URL + `/foo" }
              darwin { source = "` + f.Server.URL + `/bar" }
			}
		`,
	})
	defer f.Clean()

	_, err := f.Env.ValidateManifest(f.P, "test", &hermit.ValidationOptions{CheckSources: true})
	require.Error(t, err)
	require.Equal(t, "test-1.0.0: darwin-amd64: invalid source: could not retrieve source archive from "+f.Server.URL+"/bar: 404 Not Found", err.Error())

	_, err = f.Env.ValidateManifest(f.P, "test", &hermit.ValidationOptions{CheckSources: false})
	require.NoError(t, err)
}
