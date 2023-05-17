package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/microsoft/azure-devops-go-api/azuredevops"
	"github.com/microsoft/azure-devops-go-api/azuredevops/git"
	"github.com/microsoft/azure-devops-go-api/azuredevops/webapi"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"github.com/xanzy/go-gitlab"
	"gopkg.in/alecthomas/kingpin.v2"
	"io/ioutil"
	"regexp"
	"time"
)

var (
	gitlabToken         = kingpin.Flag("gitlab-token", "Gitlab API token").Required().String()
	azdoOrganization    = kingpin.Flag("azdo-org", "Azure DevOps organization URL (https://dev.azure.com/myorg)").Required().String()
	azdoToken           = kingpin.Flag("azdo-token", "Azure DevOps Personal Access Token").Required().String()
	azdoServiceEndpoint = kingpin.Flag("azdo-endpoint", "Azure DevOps service endpoint for gitlab").Default("").String()
	configFile          = kingpin.Flag("config", "Projects configuration file").Default("projects.json").String()
	recreateRepository  = kingpin.Flag("recreate-repo", "If true, repository in azdo will be deleted first and created again. Use with caution").Default("false").Bool()
	archiveProjects     = kingpin.Flag("archive-projects", "If true, repositories in gitlab will be archived after transition.").Default("true").Bool()
	//SuggestionReplacer Regex to match gitlab suggestion schema so that it can be replaced to azdo schema
	SuggestionReplacer = regexp.MustCompile("```suggestion:.*")
)

type config struct {
	Projects []project `json:"projects"`
}

type project struct {
	GitlabID    int    `json:"gitlabID"`
	AzdoProject string `json:"azdoProject"`
	MigrateMRs  bool   `json:"migrateMRs"`
}

func main() {
	log.AddFlags(kingpin.CommandLine)
	kingpin.HelpFlag.Short('h')
	kingpin.Version(version.Version)
	kingpin.Parse()

	gitlabClient := initGitlab()
	azdoCtx, azdoClient := initAzdo()
	configFile := readConfig()

	for i, project := range configFile.Projects {
		log.Infof("processing project %d (%d/%d)", project.GitlabID, i+1, len(configFile.Projects))
		processProject(azdoCtx, project, gitlabClient, azdoClient)
	}
}

func processProject(azdoCtx context.Context, project project, gitlabClient *gitlab.Client, azdoClient git.Client) {
	gitlabProject, _, err := gitlabClient.Projects.GetProject(project.GitlabID, &gitlab.GetProjectOptions{})
	if err != nil {
		log.Errorf("couldn't find gitlab project %d does your API key have permission to the project?", project.GitlabID)
		return
	}

	log.Debugf("creating import request for %s to project %s", gitlabProject.HTTPURLToRepo, project.AzdoProject)
	repository := importRepository(azdoCtx, project, gitlabProject, azdoClient)
	if repository == nil {
		return
	}

	if project.MigrateMRs {
		importMergeRequests(azdoCtx, project, gitlabClient, azdoClient, gitlabProject, repository)
	}

	if *archiveProjects {
		log.Debugf("archiving project %d in Gitlab", project.GitlabID)
		_, _, err := gitlabClient.Projects.ArchiveProject(project.GitlabID)
		if err != nil {
			log.Errorf("couldn't archive gitlab project %d: %s", project.GitlabID, err.Error())
		}
	}
}

func importMergeRequests(azdoCtx context.Context, project project, gitlabClient *gitlab.Client, azdoClient git.Client, gitlabProject *gitlab.Project, repository *git.GitRepository) {
	log.Debugf("migrate merge requests for repo %s", *repository.Name)
	gitlabMROptions := gitlab.ListProjectMergeRequestsOptions{
		ListOptions: gitlab.ListOptions{
			Page:    1,
			PerPage: 100,
		},
		OrderBy: gitlab.String("created_at"),
		Sort:    gitlab.String("asc"),
	}
	for {
		mergeRequests, response, err := gitlabClient.MergeRequests.ListProjectMergeRequests(gitlabProject.ID, &gitlabMROptions)
		if err != nil {
			log.Errorf("could not fetch MRs page %d: %s", gitlabMROptions.Page, err.Error())
		}
		for _, mr := range mergeRequests {
			importMergeRequest(azdoCtx, azdoClient, gitlabClient, project, mr, repository)
		}
		if response.NextPage > response.CurrentPage {
			gitlabMROptions.Page++
			continue
		}
		break
	}
}

func importMergeRequest(azdoCtx context.Context, azdoClient git.Client, gitlabClient *gitlab.Client, project project, mr *gitlab.MergeRequest, repository *git.GitRepository) {
	azdoRequest := translatePullRequest(mr, repository)
	if azdoRequest == nil {
		return
	}
	pullRequestArgs := git.CreatePullRequestArgs{
		GitPullRequestToCreate: azdoRequest,
		RepositoryId:           gitlab.String(repository.Id.String()),
		Project:                &project.AzdoProject,
		SupportsIterations:     gitlab.Bool(false),
	}

	pullRequest, err := azdoClient.CreatePullRequest(azdoCtx, pullRequestArgs)
	if err != nil {
		log.Errorf("cannot migrate merge request %d: %s", mr.IID, err.Error())
		return
	}
	importComments(azdoCtx, mr, pullRequest, gitlabClient, azdoClient)
}

func importComments(azdoCtx context.Context, mr *gitlab.MergeRequest, pullRequest *git.GitPullRequest, gitlabClient *gitlab.Client, azdoClient git.Client) {
	log.Debugf("migrate discussions for merge request %d", mr.IID)
	discussionOptions := gitlab.ListMergeRequestDiscussionsOptions{
		Page:    1,
		PerPage: 100,
	}
	for {
		discussions, response, err := gitlabClient.Discussions.ListMergeRequestDiscussions(mr.ProjectID, mr.IID, &discussionOptions)
		if err != nil {
			log.Errorf("could not fetch Discussion page %d: %s", discussionOptions.Page, err.Error())
		}
		for _, discussion := range discussions {
			importCommentThread(azdoCtx, azdoClient, mr, pullRequest, discussion)
		}
		if response.NextPage > response.CurrentPage {
			discussionOptions.Page++
			continue
		}
		break
	}
}

func importCommentThread(azdoCtx context.Context, azdoClient git.Client, mr *gitlab.MergeRequest, pullRequest *git.GitPullRequest, discussion *gitlab.Discussion) {
	threadInit, fullThread := translateDiscussion(mr, discussion)
	if threadInit == nil {
		return
	}
	threadArgs := git.CreateThreadArgs{
		CommentThread: threadInit,
		RepositoryId:  pullRequest.Repository.Name,
		PullRequestId: pullRequest.PullRequestId,
		Project:       pullRequest.Repository.Project.Name,
	}
	createdThread, err := azdoClient.CreateThread(azdoCtx, threadArgs)
	if err != nil {
		log.Errorf("cannot create thread (%s): %s", prepareNoteLink(discussion.Notes[0], mr), err)
		return
	}
	if fullThread != nil {
		fullThread.Id = createdThread.Id
		updateThreadArgs := git.UpdateThreadArgs{
			CommentThread: fullThread,
			RepositoryId:  pullRequest.Repository.Name,
			PullRequestId: pullRequest.PullRequestId,
			Project:       pullRequest.Repository.Project.Name,
			ThreadId:      createdThread.Id,
		}
		_, err = azdoClient.UpdateThread(azdoCtx, updateThreadArgs)
		if err != nil {
			log.Errorf("cannot update thread (%s): %s", prepareNoteLink(discussion.Notes[0], mr), err)
			return
		}
	}
}

func translateDiscussion(mr *gitlab.MergeRequest, discussion *gitlab.Discussion) (*git.GitPullRequestCommentThread, *git.GitPullRequestCommentThread) {
	status := git.CommentThreadStatusValues.Fixed
	firstNote := discussion.Notes[0]
	if firstNote.System {
		return nil, nil
	}
	var comments []git.Comment
	thread := git.GitPullRequestCommentThread{
		PullRequestThreadContext: nil,
		PublishedDate:            &azuredevops.Time{Time: *firstNote.CreatedAt},
	}
	if firstNote.Position != nil && firstNote.Position.NewPath != "" {
		line := firstNote.Position.NewLine
		if firstNote.Position.LineRange != nil {
			line = firstNote.Position.LineRange.StartRange.NewLine
		}
		thread.ThreadContext = &git.CommentThreadContext{
			FilePath:       gitlab.String("/" + firstNote.Position.NewPath),
			RightFileStart: &git.CommentPosition{Line: &line},
			RightFileEnd:   &git.CommentPosition{Line: &line},
		}
	}
	id := 1
	for _, note := range discussion.Notes {
		commentType := &git.CommentTypeValues.Text

		if firstNote.Position != nil && firstNote.Position.NewPath != "" {
			commentType = &git.CommentTypeValues.CodeChange
		}
		comment := translateNote(mr, note, id, commentType)
		if !note.Resolved {
			status = git.CommentThreadStatusValues.Active
		}
		comments = append(comments, comment)
		id++
	}
	thread.Status = &status
	if len(comments) == 1 {
		thread.Comments = &comments
		return &thread, nil
	}

	//thread has to exist to add replies, original comment and ThreadContext must be omitted
	threadInit := thread
	threadInit.Comments = &[]git.Comment{comments[0]}
	skipFirstComment := comments[1:]
	thread.Comments = &skipFirstComment
	thread.ThreadContext = nil

	return &threadInit, &thread
}

func translateNote(mr *gitlab.MergeRequest, note *gitlab.Note, id int, commentType *git.CommentType) git.Comment {
	content := prepareNoteBody(mr, note, id)

	comment := git.Comment{
		Id:              gitlab.Int(id),
		Content:         &content,
		PublishedDate:   &azuredevops.Time{Time: *note.CreatedAt},
		LastUpdatedDate: &azuredevops.Time{Time: *note.UpdatedAt},
		CommentType:     commentType,
	}
	comment.ParentCommentId = gitlab.Int(id - 1)
	return comment
}

func prepareNoteBody(mr *gitlab.MergeRequest, note *gitlab.Note, id int) string {
	lineRange := ""
	body := note.Body
	if id == 1 && note.Position != nil && note.Position.LineRange != nil && note.Position.LineRange.StartRange.NewLine != note.Position.LineRange.EndRange.NewLine {
		//AzDO does not support multiline comments so we add a note at least
		lineRange = fmt.Sprintf("| **🚩 Multiline comment %d-%d**", note.Position.LineRange.StartRange.NewLine, note.Position.LineRange.EndRange.NewLine)
		body = SuggestionReplacer.ReplaceAllString(body, "🚩 **️Multiline suggestions are not supported in AzDO - if suggestion is multiline, commit it manually**\n```suggestion")
	}
	body = SuggestionReplacer.ReplaceAllString(body, "```suggestion")
	content := fmt.Sprintf(
		"*Migrated from [Gitlab](%s) | Author: ![%s](%s =24x24) [%s](%s)%s*\n\n%s",
		prepareNoteLink(note, mr),
		note.Author.Name,
		note.Author.AvatarURL,
		note.Author.Name,
		note.Author.WebURL,
		lineRange,
		body,
	)
	return content
}

func prepareNoteLink(note *gitlab.Note, mr *gitlab.MergeRequest) string {
	return fmt.Sprintf("%s/diffs#note_%d", mr.WebURL, note.ID)
}

func translatePullRequest(mr *gitlab.MergeRequest, repository *git.GitRepository) *git.GitPullRequest {
	if mr.State == "closed" || mr.State == "merged" {
		return nil
	}
	azdoRequest := git.GitPullRequest{}

	azdoRequest.CreatedBy = &webapi.IdentityRef{
		DisplayName: &mr.Author.Username,
		Descriptor:  &mr.Author.Name,
	}
	azdoRequest.CreationDate = &azuredevops.Time{Time: *mr.CreatedAt}
	azdoRequest.IsDraft = &mr.WorkInProgress
	azdoRequest.Repository = repository
	if mr.MergeCommitSHA != "" {
		azdoRequest.LastMergeCommit = &git.GitCommitRef{
			CommitId: &mr.MergeCommitSHA,
		}
	}
	azdoRequest.Status = &git.PullRequestStatusValues.Active

	description := preparePullRequestDescription(mr)
	azdoRequest.Title = &mr.Title
	sourceBranch := fmt.Sprintf("refs/heads/%s", mr.SourceBranch)
	targetBranch := fmt.Sprintf("refs/heads/%s", mr.TargetBranch)
	azdoRequest.SourceRefName = &sourceBranch
	azdoRequest.TargetRefName = &targetBranch
	azdoRequest.Description = &description
	return &azdoRequest
}

func preparePullRequestDescription(mr *gitlab.MergeRequest) string {
	return fmt.Sprintf(
		"*Migrated from [Gitlab](%s) | Author: ![%s](%s =24x24) [%s](%s)*\n\n%s",
		mr.WebURL,
		mr.Author.Name,
		mr.Author.AvatarURL,
		mr.Author.Name,
		mr.Author.WebURL,
		mr.Description,
	)
}

func importRepository(azdoCtx context.Context, project project, gitlabProject *gitlab.Project, azdoClient git.Client) *git.GitRepository {
	azdoRepository, err := reinitAzdoRepository(azdoCtx, project, gitlabProject, azdoClient)
	if err != nil {
		log.Error(err)
		return nil
	}
	log.Infof("GIT:%d;%s;%s;%s;%s", gitlabProject.ID, gitlabProject.WebURL, gitlabProject.SSHURLToRepo, gitlabProject.HTTPURLToRepo, *azdoRepository.SshUrl)

	importRequest, err := createImportRequest(azdoCtx, project, gitlabProject, azdoClient, azdoRepository)
	if err != nil {
		log.Error(err)
		return nil
	}

	requestStatusArg := git.GetImportRequestArgs{
		Project:         &project.AzdoProject,
		RepositoryId:    gitlab.String(azdoRepository.Id.String()),
		ImportRequestId: importRequest.ImportRequestId,
	}
	for {
		currentRequest, err := azdoClient.GetImportRequest(azdoCtx, requestStatusArg)
		if (currentRequest == nil && err == nil) || *currentRequest.Status == git.GitAsyncOperationStatusValues.Completed {
			log.Debugf("import finished - %s", *azdoRepository.WebUrl)
			return azdoRepository
		}
		if *currentRequest.Status == git.GitAsyncOperationStatusValues.Abandoned {
			log.Error("import request abandoned")
			return nil
		}
		if *currentRequest.Status == git.GitAsyncOperationStatusValues.Failed {
			log.Errorf("import request failed: %s", *currentRequest.DetailedStatus.ErrorMessage)
			return nil
		}

		log.Debugf("waiting for import to finish retry in 3 seconds...")
		time.Sleep(3 * time.Second)
	}
}

func createImportRequest(azdoCtx context.Context, project project, gitlabProject *gitlab.Project, azdoClient git.Client, azdoRepository *git.GitRepository) (*git.GitImportRequest, error) {
	requestArg := git.GitImportRequest{
		Parameters: &git.GitImportRequestParameters{
			GitSource: &git.GitImportGitSource{
				Overwrite: gitlab.Bool(false),
				Url:       &gitlabProject.HTTPURLToRepo,
			},
		},
	}
	if *azdoServiceEndpoint != "" {
		spUUID := uuid.MustParse(*azdoServiceEndpoint)
		requestArg.Parameters.ServiceEndpointId = &spUUID
	}

	importRequestArgs := git.CreateImportRequestArgs{
		ImportRequest: &requestArg,
		Project:       &project.AzdoProject,
		RepositoryId:  gitlab.String(azdoRepository.Id.String()),
	}

	log.Debugf("create import request to transfer %s into new repo %s", gitlabProject.HTTPURLToRepo, gitlabProject.Path)
	importRequest, err := azdoClient.CreateImportRequest(azdoCtx, importRequestArgs)
	if err != nil {
		return nil, fmt.Errorf("could not create import request. Either service endpoint is not correct or source repository is empty: %s", err)
	}
	return importRequest, nil
}

func reinitAzdoRepository(azdoCtx context.Context, project project, gitlabProject *gitlab.Project, azdoClient git.Client) (*git.GitRepository, error) {
	if *recreateRepository {
		log.Debugf("removing repository %s if exists from %s", gitlabProject.Path, project.AzdoProject)
		repo, _ := azdoClient.GetRepository(azdoCtx, git.GetRepositoryArgs{
			RepositoryId: &gitlabProject.Path,
			Project:      &project.AzdoProject,
		})
		if repo != nil {
			err := azdoClient.DeleteRepository(azdoCtx, git.DeleteRepositoryArgs{
				RepositoryId: repo.Id,
				Project:      nil,
			})
			if err != nil {
				return nil, fmt.Errorf("could remove previous repository, cannot import to existing repo %s", err.Error())
			}
		}
	}

	log.Debugf("create empty repository %s", gitlabProject.Path)
	azdoRepository, err := azdoClient.CreateRepository(azdoCtx, git.CreateRepositoryArgs{
		GitRepositoryToCreate: &git.GitRepositoryCreateOptions{
			Name: &gitlabProject.Path,
		},
		Project: &project.AzdoProject,
	})
	if err != nil {
		return nil, fmt.Errorf("could not initiate repository %s: %s", gitlabProject.Path, err)
	}
	return azdoRepository, nil
}

func readConfig() config {
	file, _ := ioutil.ReadFile(*configFile)

	configFile := config{}

	err := json.Unmarshal(file, &configFile)
	if err != nil {
		log.Fatal(err)
	}
	return configFile
}

func initAzdo() (context.Context, git.Client) {
	connection := azuredevops.NewPatConnection(*azdoOrganization, *azdoToken)

	ctx := context.Background()

	client, err := git.NewClient(ctx, connection)
	if err != nil {
		log.Fatal(err)
	}
	return ctx, client
}

func initGitlab() *gitlab.Client {
	gitlabClient, err := gitlab.NewClient(*gitlabToken)
	if err != nil {
		log.Fatal(err)
	}
	return gitlabClient
}
