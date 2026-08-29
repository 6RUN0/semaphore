package db

import (
	"math/rand"
	"os"
	"path"
	"testing"

	"github.com/semaphoreui/semaphore/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepository_GetSchema(t *testing.T) {
	repo := Repository{GitURL: "https://example.com/hello/world"}
	schema := repo.GetType()
	assert.Equal(t, RepositoryHTTP, schema)
}

func TestRepository_GetType_WindowsLocalPath(t *testing.T) {
	assert.Equal(t, RepositoryLocal, Repository{GitURL: `D:\repo`}.GetType())
	assert.Equal(t, RepositoryLocal, Repository{GitURL: `D:/repo`}.GetType())
	assert.Equal(t, RepositoryLocal, Repository{GitURL: `D:`}.GetType())
	assert.Equal(t, RepositoryLocal, Repository{GitURL: `\\server\share`}.GetType())
}

func TestRepository_ClearCache(t *testing.T) {
	util.Config = &util.ConfigType{
		TmpPath: path.Join(os.TempDir(), util.RandString(rand.Intn(10-4)+4)),
	}
	repoDir := path.Join(util.Config.TmpPath, "project_0", "repository_123_55")
	err := os.MkdirAll(repoDir, 0755)
	require.NoError(t, err)

	repo := Repository{ID: 123}
	err = repo.ClearCache()
	require.NoError(t, err)

	_, err = os.Stat(repoDir)
	require.Error(t, err, "repo directory not deleted")
	assert.True(t, os.IsNotExist(err))
}

func TestRepository_GetGitURL(t *testing.T) {
	for _, v := range []struct {
		Repository     Repository
		ExpectedGitUrl string
	}{
		{
			Repository: Repository{GitURL: "https://github.com/user/project.git", SSHKey: AccessKey{
				Type: AccessKeyLoginPassword,
				LoginPassword: LoginPassword{
					Login:    "login",
					Password: "password",
				},
			},
			},
			ExpectedGitUrl: "https://login:password@github.com/user/project.git",
		},
		{
			Repository: Repository{GitURL: "https://github.com/user/project.git", SSHKey: AccessKey{
				Type: AccessKeyLoginPassword,
				LoginPassword: LoginPassword{
					Password: "password",
				},
			},
			},
			ExpectedGitUrl: "https://password@github.com/user/project.git",
		},
	} {
		gitUrl := v.Repository.GetGitURL(false)
		assert.Equal(t, v.ExpectedGitUrl, gitUrl, "wrong gitUrl")
	}
}

func TestRepository_Validate(t *testing.T) {
	tests := []struct {
		name    string
		repo    Repository
		wantErr string
	}{
		{"remote with branch", Repository{Name: "r", GitURL: "https://example.com/x.git", GitBranch: "main"}, ""},
		// A local repository is a directory on the node: there is no branch to
		// check out, and demanding one would make it unusable.
		{"local without branch", Repository{Name: "r", GitURL: "/srv/repos/x"}, ""},
		{"local windows path without branch", Repository{Name: "r", GitURL: `D:\repos\x`}, ""},

		{"no name", Repository{GitURL: "https://example.com/x.git", GitBranch: "main"}, "name can't be empty"},
		{"no url", Repository{Name: "r", GitBranch: "main"}, "url can't be empty"},
		{"url reads as a command line option", Repository{Name: "r", GitURL: "--upload-pack=evil", GitBranch: "main"}, "url is invalid"},
		{"remote without branch", Repository{Name: "r", GitURL: "https://example.com/x.git"}, "branch can't be empty"},
		{"branch name git rejects", Repository{Name: "r", GitURL: "https://example.com/x.git", GitBranch: "bad branch"}, "branch name is invalid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.repo.Validate()

			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}
