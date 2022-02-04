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

	for _, project := range configFile.Projects {
		processProject(azdoCtx, project, gitlabClient, azdoClient)
	}
}

func processProject(azdoCtx context.Context, project project, gitlabClient *gitlab.Client, azdoClient git.Client) {
	gitlabProject, _, err := gitlabClient.Projects.GetProject(project.GitlabID, &gitlab.GetProjectOptions{})
	if err != nil {
		log.Errorf("Couldn't find gitlab project %d does your API key have permission to the project?", project.GitlabID)
		return
	}

	log.Debugf("Creating import request for %s to project %s", gitlabProject.HTTPURLToRepo, project.AzdoProject)
	repository := importRepository(azdoCtx, project, gitlabProject, azdoClient)
	if repository == nil {
		return
	}

	if project.MigrateMRs {
		importMergeRequests(azdoCtx, project, gitlabClient, azdoClient, gitlabProject, repository)
	}
}

func importMergeRequests(azdoCtx context.Context, project project, gitlabClient *gitlab.Client, azdoClient git.Client, gitlabProject *gitlab.Project, repository *git.GitRepository) {
	log.Debugf("Migrate merge requests for repo %s", *repository.Name)
	gitlabMROptions := gitlab.ListProjectMergeRequestsOptions{}
	for {
		mergeRequests, response, _ := gitlabClient.MergeRequests.ListProjectMergeRequests(gitlabProject.ID, &gitlabMROptions)
		for _, mr := range mergeRequests {
			if mr.State == "closed" || mr.State == "merged" {
				continue
			}
			azdoRequest := translatePullRequest(mr, repository)
			pullRequestArgs := git.CreatePullRequestArgs{
				GitPullRequestToCreate: &azdoRequest,
				RepositoryId:           gitlab.String(repository.Id.String()),
				Project:                &project.AzdoProject,
				SupportsIterations:     gitlab.Bool(false),
			}

			pullRequest, err := azdoClient.CreatePullRequest(azdoCtx, pullRequestArgs)
			if err != nil {
				log.Error(err)
			}
			importComments(azdoCtx, mr, pullRequest, gitlabClient, azdoClient)
		}
		if response.NextPage > response.CurrentPage {
			gitlabMROptions.Page++
			continue
		}
		break
	}
}

func importComments(azdoCtx context.Context, mr *gitlab.MergeRequest, pullRequest *git.GitPullRequest, gitlabClient *gitlab.Client, azdoClient git.Client) {
	log.Debugf("Migrate discussions for merge request %d", mr.IID)
	discussionOptions := gitlab.ListMergeRequestDiscussionsOptions{}
	for {
		discussions, response, _ := gitlabClient.Discussions.ListMergeRequestDiscussions(mr.ProjectID, mr.IID, &discussionOptions)
		for _, discussion := range discussions {
			threadInit, fullThread := translateDiscussion(mr, discussion)
			if threadInit == nil {
				continue
			}
			threadArgs := git.CreateThreadArgs{
				CommentThread: threadInit,
				RepositoryId:  pullRequest.Repository.Name,
				PullRequestId: pullRequest.PullRequestId,
				Project:       pullRequest.Repository.Project.Name,
			}
			createdThread, err := azdoClient.CreateThread(azdoCtx, threadArgs)
			if err != nil {
				log.Error(err)
				continue
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
					log.Error(err)
					continue
				}
			}
		}
		if response.NextPage > response.CurrentPage {
			discussionOptions.Page++
			continue
		}
		break
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
	}
	if firstNote.Position != nil && firstNote.Position.NewPath != "" {
		thread.ThreadContext = &git.CommentThreadContext{
			FilePath:       gitlab.String("/" + firstNote.Position.NewPath),
			RightFileStart: &git.CommentPosition{Line: &firstNote.Position.LineRange.StartRange.NewLine},
			RightFileEnd:   &git.CommentPosition{Line: &firstNote.Position.LineRange.StartRange.NewLine},
		}
	}
	id := 1
	suggestionReplacer := regexp.MustCompile("```suggestion:.*")
	for _, note := range discussion.Notes {
		lineRange := ""
		body := note.Body
		if id == 1 && note.Position != nil && note.Position.LineRange.StartRange.NewLine != note.Position.LineRange.EndRange.NewLine {
			//AzDO does not support multiline comments so we add a note at least
			lineRange = fmt.Sprintf("| **🚩 Multiline comment %d-%d**", note.Position.LineRange.StartRange.NewLine, note.Position.LineRange.EndRange.NewLine)
			body = suggestionReplacer.ReplaceAllString(body, "🚩 **️Multiline suggestions are not supported in AzDO - if suggestion is multiline, commit it manually**\n```suggestion")
		}
		body = suggestionReplacer.ReplaceAllString(body, "```suggestion")
		content := fmt.Sprintf(
			"*Migrated from [Gitlab](%s/diffs#note_%d) | Author: ![%s](%s =24x24) [%s](%s)%s*\n\n%s",
			mr.WebURL,
			note.ID,
			note.Author.Name,
			note.Author.AvatarURL,
			note.Author.Name,
			note.Author.WebURL,
			lineRange,
			body,
		)

		comment := git.Comment{
			Id:              gitlab.Int(id),
			Content:         &content,
			PublishedDate:   &azuredevops.Time{Time: *note.CreatedAt},
			LastUpdatedDate: &azuredevops.Time{Time: *note.UpdatedAt},
			CommentType:     &git.CommentTypeValues.Text,
		}
		if firstNote.Position != nil && firstNote.Position.NewPath != "" {
			comment.CommentType = &git.CommentTypeValues.CodeChange
		}
		comment.ParentCommentId = gitlab.Int(id - 1)
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

func translatePullRequest(mr *gitlab.MergeRequest, repository *git.GitRepository) git.GitPullRequest {
	azdoRequest := git.GitPullRequest{}
	description := mr.Description

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

	description = fmt.Sprintf(
		"*Migrated from [Gitlab](%s) | Author: ![%s](%s =24x24) [%s](%s)*\n\n%s",
		mr.WebURL,
		mr.Author.Name,
		mr.Author.AvatarURL,
		mr.Author.Name,
		mr.Author.WebURL,
		description,
	)
	azdoRequest.Title = &mr.Title
	sourceBranch := fmt.Sprintf("refs/heads/%s", mr.SourceBranch)
	targetBranch := fmt.Sprintf("refs/heads/%s", mr.TargetBranch)
	azdoRequest.SourceRefName = &sourceBranch
	azdoRequest.TargetRefName = &targetBranch
	azdoRequest.Description = &description
	return azdoRequest
}

func importRepository(azdoCtx context.Context, project project, gitlabProject *gitlab.Project, azdoClient git.Client) *git.GitRepository {
	if *recreateRepository {
		log.Debugf("Removing repository %s if exists from %s", gitlabProject.Path, project.AzdoProject)
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
				log.Errorf("Could remove previous repository, cannot import to existing repo %s", err.Error())
			}
		}
	}

	log.Debugf("Create empty repository %s", gitlabProject.Path)
	azdoRepository, err := azdoClient.CreateRepository(azdoCtx, git.CreateRepositoryArgs{
		GitRepositoryToCreate: &git.GitRepositoryCreateOptions{
			Name: &gitlabProject.Path,
		},
		Project: &project.AzdoProject,
	})
	if err != nil {
		log.Error(err)
		return nil
	}

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

	log.Debugf("Create import request to transfer %s into new repo %s", gitlabProject.HTTPURLToRepo, gitlabProject.Path)
	importRequest, err := azdoClient.CreateImportRequest(azdoCtx, importRequestArgs)
	if err != nil {
		log.Errorf("could not create import request. If you're using private repo you need to specify service endpoint for gitlab (see README): %s", err)
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
			log.Debug("Import finished")
			return azdoRepository
		}
		if *currentRequest.Status == git.GitAsyncOperationStatusValues.Abandoned {
			log.Error("Import request abandoned")
			return nil
		}
		if *currentRequest.Status == git.GitAsyncOperationStatusValues.Failed {
			log.Errorf("Import request failed: %s", *currentRequest.DetailedStatus.ErrorMessage)
			return nil
		}

		log.Debugf("Waiting for import to finish retry in 3 seconds...")
		time.Sleep(3 * time.Second)
	}
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
