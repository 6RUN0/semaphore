package db_lib

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/semaphoreui/semaphore/pkg/ssh"

	"github.com/semaphoreui/semaphore/db"
	"github.com/semaphoreui/semaphore/util"

	log "github.com/sirupsen/logrus"
)

type CmdGitClient struct {
	keyInstaller AccessKeyInstaller
}

func (c CmdGitClient) makeCmd(
	r GitRepository,
	targetDir GitRepositoryDirType,
	installation ssh.AccessKeyInstallation,
	args ...string,
) *exec.Cmd {
	cmd := exec.Command("git") //nolint: gas

	cmd.Env = append(getEnvironmentVars(), installation.GetGitEnv()...)

	switch targetDir {
	case GitRepositoryTmpPath:
		cmd.Dir = util.Config.GetProjectTmpDir(r.Repository.ProjectID)
		_, err := os.Stat(cmd.Dir)
		if err != nil {
			if os.IsNotExist(err) {
				err = os.MkdirAll(cmd.Dir, 0755)
				if err != nil {
					log.WithError(err).WithFields(log.Fields{
						"context": "git",
					}).Error("failed to create project temp directory")
				}
			} else {
				log.WithError(err).WithFields(log.Fields{
					"context": "git",
				}).Error("failed to check existing project temp directory")
			}
		}
	case GitRepositoryFullPath:
		cmd.Dir = r.GetFullPath()
	default:
		panic("unknown Repository directory type")
	}

	cmd.Args = append(cmd.Args, args...)

	cmd.SysProcAttr = util.Config.GetSysProcAttr()

	return cmd
}

func (c CmdGitClient) run(r GitRepository, targetDir GitRepositoryDirType, args ...string) error {
	var err error
	keyInstallation, err := c.keyInstaller.Install(r.Repository.SSHKey, db.AccessKeyRoleGit, r.Logger)

	if err != nil {
		return err
	}

	defer keyInstallation.Destroy() //nolint: errcheck

	cmd := c.makeCmd(r, targetDir, keyInstallation, args...)

	boundOutputDrain(cmd)
	finishLog := r.Logger.LogCmd(cmd)
	defer finishLog()

	err = cmd.Run()
	finishLog() // Flush the command output before the drain notice, if any.

	return acceptDrainTimeout(r.Logger, err)
}

// lsRemoteTimeout bounds a `git ls-remote`.
//
// It is the one git call made while an HTTP request waits on it — the inventory
// snapshot read path asks for the remote head, and the repository branch list
// is served the same way — and git has no timeout of its own. A host that
// accepts the connection and then answers nothing leaves the request goroutine
// and the git process waiting for as long as the client is willing to wait,
// and every retry adds another pair. The other git operations are not bounded:
// a clone or a pull is as long as the repository is big, and only this one has
// a size that does not depend on the repository.
const lsRemoteTimeout = 2 * time.Minute

// killGrace bounds the wait for a killed command to be reaped. On Unix the kill
// reaches the whole process group and Wait returns at once; on Windows a
// surviving grandchild can hold the output pipe open, and waiting on it forever
// is what this avoids.
const killGrace = 5 * time.Second

// runBounded runs cmd and returns what it wrote to standard output, killing it
// and everything it spawned once timeout expires.
func runBounded(cmd *exec.Cmd, timeout time.Duration) ([]byte, error) {
	// Own process group: killing only the direct child leaves whatever it
	// spawned (ssh, a credential helper) holding the inherited output pipe,
	// and Wait would then block on that pipe rather than return.
	isolateProcessGroup(cmd)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return stdout.Bytes(), err
	case <-time.After(timeout):
		killProcessGroup(cmd)

		select {
		case <-done:
		case <-time.After(killGrace):
		}

		subcommand := "command"
		if len(cmd.Args) > 1 {
			subcommand = cmd.Args[1]
		}

		return nil, fmt.Errorf("git %s timed out after %s", subcommand, timeout)
	}
}

func (c CmdGitClient) outputWithin(
	r GitRepository,
	targetDir GitRepositoryDirType,
	timeout time.Duration,
	args ...string,
) (out string, err error) {
	keyInstallation, err := c.keyInstaller.Install(r.Repository.SSHKey, db.AccessKeyRoleGit, r.Logger)
	if err != nil {
		return
	}

	defer keyInstallation.Destroy() //nolint: errcheck

	bytes, err := runBounded(c.makeCmd(r, targetDir, keyInstallation, args...), timeout)
	if err != nil {
		return
	}

	out = strings.Trim(string(bytes), " \n")
	return
}

func (c CmdGitClient) output(r GitRepository, targetDir GitRepositoryDirType, args ...string) (out string, err error) {
	keyInstallation, err := c.keyInstaller.Install(r.Repository.SSHKey, db.AccessKeyRoleGit, r.Logger)
	if err != nil {
		return
	}

	defer keyInstallation.Destroy() //nolint: errcheck

	bytes, err := c.makeCmd(r, targetDir, keyInstallation, args...).Output()
	if err != nil {
		return
	}
	out = strings.Trim(string(bytes), " \n")
	return
}

func (c CmdGitClient) Clone(r GitRepository) error {
	r.Logger.Log("Cloning Repository " + r.Repository.GitURL)

	var dirName string
	if r.TmpDirName == "" {
		dirName = r.Repository.GetDirName(r.TemplateID)
	} else {
		dirName = r.TmpDirName
	}

	targetPath := r.GetFullPath()
	if err := os.MkdirAll(targetPath, 0755); err != nil {
		return err
	}
	if err := util.ChownDir(targetPath); err != nil {
		return err
	}

	return c.run(r, GitRepositoryTmpPath,
		gitCloneArgs(
			r.Repository.GitBranch,
			r.Repository.GetGitURL(false),
			dirName,
			util.Config.GitSubmoduleJobs,
		)...)
}

// gitSubmoduleJobsArgs builds the --jobs argument of a submodule-aware git
// command, and nothing at all when the count is below one.
//
// git refuses to run with `--jobs 0`: it aborts with "you must provide a
// non-zero number of processes" (exit code 134 on git 2.54), and a negative
// count ends the command as well. Config validation keeps the configured value
// within 1..maxGitSubmoduleJobs, but util.Config is a plain struct that any
// code path can fill in without ConfigInit, so the zero value does reach here.
// Without the option git falls back to submodule.fetchJobs, or to one job.
//
// A count above the ceiling is passed on unchanged: clamping it here would
// silently disagree with the value the administrator set, and the ceiling is
// enforced where the config is validated.
func gitSubmoduleJobsArgs(jobs int) []string {
	if jobs < 1 {
		return nil
	}

	return []string{"--jobs", strconv.Itoa(jobs)}
}

// gitCloneArgs builds a recursive clone. --end-of-options separates the
// options from the URL, so that a repository address beginning with a dash is
// not read as one.
func gitCloneArgs(branch, url, dirName string, submoduleJobs int) []string {
	args := []string{"clone", "--recursive"}
	args = append(args, gitSubmoduleJobsArgs(submoduleJobs)...)
	return append(args, "--branch", branch, "--end-of-options", url, dirName)
}

// gitSubmoduleUpdateArgs builds the submodule checkout that follows a pull.
func gitSubmoduleUpdateArgs(submoduleJobs int) []string {
	args := []string{"submodule", "update", "--init", "--recursive"}
	return append(args, gitSubmoduleJobsArgs(submoduleJobs)...)
}

func (c CmdGitClient) Pull(r GitRepository) error {
	r.Logger.Log("Updating Repository " + r.Repository.GitURL)

	err := c.run(r, GitRepositoryFullPath, "pull", "origin", "--end-of-options", r.Repository.GitBranch)
	if err != nil {
		return err
	}
	return c.run(r, GitRepositoryFullPath, gitSubmoduleUpdateArgs(util.Config.GitSubmoduleJobs)...)
}

func (c CmdGitClient) Checkout(r GitRepository, target string) error {
	r.Logger.Log("Checkout repository to " + target)

	return c.run(r, GitRepositoryFullPath, "checkout", "--end-of-options", target)
}

func (c CmdGitClient) CanBePulled(r GitRepository) bool {
	err := c.run(r, GitRepositoryFullPath, "fetch")
	if err != nil {
		return false
	}

	err = c.run(r, GitRepositoryFullPath,
		"merge-base", "--is-ancestor", "HEAD", "origin/"+r.Repository.GitBranch)

	return err == nil
}

func (c CmdGitClient) GetLastCommitMessage(r GitRepository) (msg string, err error) {
	r.Logger.Log("Get current commit message")

	msg, err = c.output(r, GitRepositoryFullPath, "show-branch", "--no-name", "HEAD")
	if err != nil {
		return
	}

	msg = truncateCommitMessage(msg)

	return
}

func (c CmdGitClient) GetLastCommitHash(r GitRepository) (hash string, err error) {
	r.Logger.Log("Get current commit hash")
	hash, err = c.output(r, GitRepositoryFullPath, "rev-parse", "HEAD")
	return
}

func (c CmdGitClient) GetLastRemoteCommitHash(r GitRepository) (hash string, err error) {
	out, err := c.outputWithin(r, GitRepositoryTmpPath, lsRemoteTimeout,
		"ls-remote", "--end-of-options", r.Repository.GetGitURL(false), r.Repository.GitBranch)
	if err != nil {
		return
	}

	firstSpaceIndex := strings.IndexAny(out, "\t ")
	if firstSpaceIndex == -1 {
		err = fmt.Errorf("can't retreave remote commit hash")
	}
	if err != nil {
		return
	}

	hash = out[0:firstSpaceIndex]
	return
}

func (c CmdGitClient) GetRemoteBranches(r GitRepository) ([]string, error) {
	out, err := c.outputWithin(r, GitRepositoryTmpPath, lsRemoteTimeout,
		"ls-remote", "--heads", "--end-of-options", r.Repository.GetGitURL(false))
	if err != nil {
		return nil, err
	}

	if len(out) == 0 {
		return []string{}, nil
	}

	branches := strings.Split(out, "\n")
	branchNames := getRepositoryBranchNames(branches)
	return branchNames, nil
}

func getRepositoryBranchNames(branches []string) []string {
	branchNames := make([]string, 0, len(branches))

	for _, branch := range branches {
		parts := strings.Split(branch, "\t")
		if len(parts) < 2 {
			continue
		}

		refPath := strings.TrimSpace(parts[1])

		const refsHeadsPrefix = "refs/heads/"
		if strings.HasPrefix(refPath, refsHeadsPrefix) {
			branchName := strings.TrimPrefix(refPath, refsHeadsPrefix)
			branchNames = append(branchNames, branchName)
		}
	}

	return branchNames
}
