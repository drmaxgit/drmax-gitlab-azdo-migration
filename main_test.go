package main

import (
	"fmt"
	"github.com/go-test/deep"
	"github.com/microsoft/azure-devops-go-api/azuredevops"
	"github.com/microsoft/azure-devops-go-api/azuredevops/git"
	"github.com/microsoft/azure-devops-go-api/azuredevops/webapi"
	"github.com/xanzy/go-gitlab"
	"testing"
	"time"
)

func TestTranslateDiscussion(t *testing.T) {
	mr := setupSimpleMergeRequest()
	_, createdAt := setupDates()
	singleComment := setupExpectedSingleComment()
	suggestionComment := setupExpectedSuggestionComment()
	singleCommentReply := setupExpectedSingleCommentReply()
	singleNote := setupSingleNote()
	suggestionNote := setupSuggestionNote()

	discussions := []struct {
		label      string
		discussion gitlab.Discussion
		init       *git.GitPullRequestCommentThread
		full       *git.GitPullRequestCommentThread
	}{
		{
			"system note - should be skipped",
			gitlab.Discussion{Notes: []*gitlab.Note{{System: true}}},
			nil,
			nil,
		},
		{
			"single generic comment",
			gitlab.Discussion{Notes: []*gitlab.Note{&singleNote}},
			&git.GitPullRequestCommentThread{
				PublishedDate: &azuredevops.Time{Time: createdAt},
				Comments:      &[]git.Comment{singleComment},
				Status:        &git.CommentThreadStatusValues.Active,
			},
			nil,
		},
		{
			"thread discussion",
			gitlab.Discussion{Notes: []*gitlab.Note{&suggestionNote, &singleNote}},
			&git.GitPullRequestCommentThread{
				PublishedDate: &azuredevops.Time{Time: createdAt},
				Comments:      &[]git.Comment{suggestionComment},
				Status:        &git.CommentThreadStatusValues.Active,
				ThreadContext: &git.CommentThreadContext{
					FilePath:       gitlab.String("/" + suggestionNote.Position.NewPath),
					RightFileStart: &git.CommentPosition{Line: &suggestionNote.Position.NewLine},
					RightFileEnd:   &git.CommentPosition{Line: &suggestionNote.Position.NewLine},
				},
			},
			&git.GitPullRequestCommentThread{
				PublishedDate: &azuredevops.Time{Time: createdAt},
				Comments:      &[]git.Comment{singleCommentReply},
				Status:        &git.CommentThreadStatusValues.Active,
			},
		},
	}

	for _, discussion := range discussions {
		threadInit, fullThread := translateDiscussion(&mr, &discussion.discussion)

		if diffInit := deep.Equal(threadInit, discussion.init); diffInit != nil {
			t.Errorf("%s: %+v", discussion.label, diffInit)
		}
		if diffFull := deep.Equal(fullThread, discussion.full); diffFull != nil {
			t.Errorf("%s: %+v", discussion.label, diffFull)
		}
	}
}

func TestTranslatePullRequest(t *testing.T) {
	openPullRequest := setupExpectedOpenPullRequest()
	pullRequests := []struct {
		label        string
		mergeRequest gitlab.MergeRequest
		pullRequest  *git.GitPullRequest
	}{
		{
			"open merge request",
			setupOpenMergeRequest(),
			&openPullRequest,
		},
		{
			"closed merge request",
			setupClosedMergeRequest(),
			nil,
		},
	}
	repository := setupExpectedRepository()

	for _, pullRequest := range pullRequests {
		pr := translatePullRequest(&pullRequest.mergeRequest, &repository)
		if diffInit := deep.Equal(pr, pullRequest.pullRequest); diffInit != nil {
			t.Errorf("%s: %+v", pullRequest.label, diffInit)
		}
	}

}
func TestPrepareNoteBody(t *testing.T) {
	expect := "*Migrated from [Gitlab](https://gitlab.com/gitlab-examples/php/-/merge_requests/1/diffs#note_0) | Author: ![John Doe](https://www.gravatar.com/avatar/0 =24x24) [John Doe](https://gitlab.com/john-doe)| **🚩 Multiline comment 1-2***\n\n🚩 **️Multiline suggestions are not supported in AzDO - if suggestion is multiline, commit it manually**\n```suggestion\nfoo\nbar\n```"
	mr := setupOpenMergeRequest()
	note := setupSuggestionNote()
	if diff := deep.Equal(expect, prepareNoteBody(&mr, &note, 1)); diff != nil {
		t.Error(diff)
	}
}

func TestPreparePullRequestDescription(t *testing.T) {
	expect := "*Migrated from [Gitlab](https://gitlab.com/gitlab-examples/php/-/merge_requests/1) | Author: ![John Doe](https://www.gravatar.com/avatar/0 =24x24) [John Doe](https://gitlab.com/john-doe)*\n\nopen merge request description"
	mr := setupOpenMergeRequest()
	if diff := deep.Equal(expect, preparePullRequestDescription(&mr)); diff != nil {
		t.Error(diff)
	}
}

func setupExpectedOpenPullRequest() git.GitPullRequest {
	_, createdAt := setupDates()
	author := setupAuthor()
	mr := setupOpenMergeRequest()
	description := preparePullRequestDescription(&mr)
	repository := setupExpectedRepository()
	return git.GitPullRequest{
		CreatedBy: &webapi.IdentityRef{
			DisplayName: &author.Username,
			Descriptor:  &author.Name,
		},
		CreationDate:    &azuredevops.Time{Time: createdAt},
		Description:     &description,
		IsDraft:         gitlab.Bool(true),
		LastMergeCommit: &git.GitCommitRef{CommitId: &mr.MergeCommitSHA},
		Repository:      &repository,
		SourceRefName:   gitlab.String(fmt.Sprintf("refs/heads/%s", mr.SourceBranch)),
		Status:          &git.PullRequestStatusValues.Active,
		TargetRefName:   gitlab.String(fmt.Sprintf("refs/heads/%s", mr.TargetBranch)),
		Title:           &mr.Title,
	}
}

func setupExpectedRepository() git.GitRepository {
	return git.GitRepository{
		Name: gitlab.String("test-repository"),
	}
}

func setupOpenMergeRequest() gitlab.MergeRequest {
	author := setupAuthor()
	_, createdAt := setupDates()
	return gitlab.MergeRequest{
		State:       "open",
		Description: "open merge request description",
		Author: &gitlab.BasicUser{
			Username:  author.Username,
			Name:      author.Name,
			AvatarURL: author.AvatarURL,
			WebURL:    author.WebURL,
		},
		CreatedAt:      &createdAt,
		WorkInProgress: true,
		MergeCommitSHA: "e83c5163316f89bfbde7d9ab23ca2e25604af290",
		WebURL:         "https://gitlab.com/gitlab-examples/php/-/merge_requests/1",
		Title:          "Foo",
		SourceBranch:   "develop",
		TargetBranch:   "master",
	}
}

func setupClosedMergeRequest() gitlab.MergeRequest {
	mr := setupOpenMergeRequest()
	mr.State = "closed"
	return mr
}

func setupExpectedSingleCommentReply() git.Comment {
	comment := setupExpectedSingleComment()
	comment.CommentType = &git.CommentTypeValues.CodeChange
	comment.Id = gitlab.Int(2)
	comment.ParentCommentId = gitlab.Int(1)
	return comment
}

func setupExpectedSingleComment() git.Comment {
	mr := setupSimpleMergeRequest()
	updatedAt, createdAt := setupDates()
	note := setupSingleNote()
	content := prepareNoteBody(&mr, &note, 1)
	return git.Comment{
		Id:              gitlab.Int(1),
		Content:         &content,
		PublishedDate:   &azuredevops.Time{Time: createdAt},
		LastUpdatedDate: &azuredevops.Time{Time: updatedAt},
		CommentType:     &git.CommentTypeValues.Text,
		ParentCommentId: gitlab.Int(0),
	}
}

func setupExpectedSuggestionComment() git.Comment {
	mr := setupSimpleMergeRequest()
	updatedAt, createdAt := setupDates()
	note := setupSuggestionNote()
	content := prepareNoteBody(&mr, &note, 1)
	return git.Comment{
		Id:              gitlab.Int(1),
		Content:         &content,
		PublishedDate:   &azuredevops.Time{Time: createdAt},
		LastUpdatedDate: &azuredevops.Time{Time: updatedAt},
		CommentType:     &git.CommentTypeValues.CodeChange,
		ParentCommentId: gitlab.Int(0),
	}
}

func setupDates() (time.Time, time.Time) {
	updatedAt := time.Date(2019, 11, 4, 15, 39, 03, 935000000, time.UTC)
	createdAt := time.Date(2019, 11, 4, 15, 38, 53, 154000000, time.UTC)
	return updatedAt, createdAt
}

func setupAuthor() struct {
	ID        int    `json:"id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	State     string `json:"state"`
	AvatarURL string `json:"avatar_url"`
	WebURL    string `json:"web_url"`
} {
	author := struct {
		ID        int    `json:"id"`
		Username  string `json:"username"`
		Email     string `json:"email"`
		Name      string `json:"name"`
		State     string `json:"state"`
		AvatarURL string `json:"avatar_url"`
		WebURL    string `json:"web_url"`
	}{
		Username:  "john-doe",
		Name:      "John Doe",
		AvatarURL: "https://www.gravatar.com/avatar/0",
		WebURL:    "https://gitlab.com/john-doe",
	}
	return author
}

func setupSimpleMergeRequest() gitlab.MergeRequest {
	return gitlab.MergeRequest{
		WebURL: "https://gitlab.com/gitlab-examples/php/-/merge_requests/1",
	}
}
func setupSuggestionNote() gitlab.Note {
	updatedAt, createdAt := setupDates()
	note := gitlab.Note{
		Body:      "```suggestion:-1+0\nfoo\nbar\n```",
		Author:    setupAuthor(),
		CreatedAt: &createdAt,
		UpdatedAt: &updatedAt,
		System:    false,
		Position: &gitlab.NotePosition{
			NewPath: "README.md",
			NewLine: 1,
			LineRange: &gitlab.LineRange{
				StartRange: &gitlab.LinePosition{NewLine: 1},
				EndRange:   &gitlab.LinePosition{NewLine: 2},
			},
		},
	}
	return note
}

func setupSingleNote() gitlab.Note {
	updatedAt, createdAt := setupDates()
	return gitlab.Note{
		System:    false,
		Body:      "single generic comment",
		Author:    setupAuthor(),
		CreatedAt: &createdAt,
		UpdatedAt: &updatedAt,
	}
}
