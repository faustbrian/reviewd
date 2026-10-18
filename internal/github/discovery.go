package github

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DiscoveredPull is inexpensive list metadata, not a complete source snapshot.
type DiscoveredPull struct {
	Pull
	RequestedUsers []Identity      `json:"requested_users"`
	RequestedTeams []RequestedTeam `json:"requested_teams"`
}
type RequestedTeam struct {
	ID     int64          `json:"id"`
	NodeID string         `json:"node_id"`
	Slug   string         `json:"slug"`
	Parent *RequestedTeam `json:"parent"`
}

// OpenPulls enumerates one exact repository without the search API's result cap.
func (c *Client) OpenPulls(ctx context.Context, repository string) (int64, []DiscoveredPull, error) {
	canonical, prefix, err := c.repositoryScope(repository)
	if err != nil {
		return 0, nil, err
	}
	var metadata struct {
		ID   int64  `json:"id"`
		Name string `json:"full_name"`
	}
	if err := c.get(ctx, prefix, &metadata); err != nil {
		return 0, nil, err
	}
	if metadata.ID <= 0 || !strings.EqualFold(metadata.Name, strings.TrimPrefix(canonical, c.host+"/")) {
		return 0, nil, errors.New("discovery repository identity changed")
	}
	pages, err := c.rawPages(ctx, prefix+"/pulls")
	if err != nil {
		return 0, nil, err
	}
	result := []DiscoveredPull{}
	ids, numbers := map[int64]bool{}, map[int]bool{}
	for _, raw := range pages {
		var row struct {
			ID        int64     `json:"id"`
			Number    int       `json:"number"`
			State     string    `json:"state"`
			Draft     *bool     `json:"draft"`
			Title     *string   `json:"title"`
			Body      string    `json:"body"`
			UpdatedAt time.Time `json:"updated_at"`
			Author    Identity  `json:"user"`
			Base      struct {
				SHA  string `json:"sha"`
				Ref  string `json:"ref"`
				Repo struct {
					ID   int64  `json:"id"`
					Name string `json:"full_name"`
				} `json:"repo"`
			} `json:"base"`
			Head struct {
				SHA string `json:"sha"`
				Ref string `json:"ref"`
			} `json:"head"`
			Users *[]Identity      `json:"requested_reviewers"`
			Teams *[]RequestedTeam `json:"requested_teams"`
		}
		if json.Unmarshal(raw, &row) != nil || row.ID <= 0 || row.Number <= 0 || row.State != "open" || row.Draft == nil || row.Title == nil || row.Users == nil || row.Teams == nil || row.Author.ID <= 0 || row.Author.Login == "" || row.UpdatedAt.IsZero() || !objectPattern.MatchString(row.Base.SHA) || !objectPattern.MatchString(row.Head.SHA) || row.Base.Ref == "" || row.Base.Repo.ID != metadata.ID || !strings.EqualFold(row.Base.Repo.Name, metadata.Name) || ids[row.ID] || numbers[row.Number] {
			return 0, nil, errors.New("pull discovery returned incomplete, repeated or mismatched identity evidence")
		}
		ids[row.ID], numbers[row.Number] = true, true
		for _, user := range *row.Users {
			if user.ID <= 0 || user.Login == "" {
				return 0, nil, errors.New("requested reviewer identity is incomplete")
			}
		}
		for _, team := range *row.Teams {
			if team.ID <= 0 || team.NodeID == "" || team.Slug == "" {
				return 0, nil, errors.New("requested team identity is incomplete")
			}
		}
		result = append(result, DiscoveredPull{Pull: Pull{PullReference: PullReference{Repository: canonical, Number: row.Number, URL: "https://" + canonical + "/pull/" + strconv.Itoa(row.Number)}, RepositoryID: metadata.ID, ID: row.ID, State: row.State, Draft: *row.Draft, Title: *row.Title, Body: row.Body, UpdatedAt: row.UpdatedAt, Author: row.Author, BaseRef: row.Base.Ref, HeadRef: row.Head.Ref, TargetBaseSHA: row.Base.SHA, HeadSHA: row.Head.SHA}, RequestedUsers: *row.Users, RequestedTeams: *row.Teams})
	}
	return metadata.ID, result, nil
}

// TeamMember checks a repository-provided requested team directly, including
// inherited membership. A denied or incomplete read is not "not a member".
const teamMembersQuery = `query ReviewdTeamMembership($id:ID!,$login:String!,$after:String){node(id:$id){... on Team{id databaseId members(first:100,after:$after,query:$login,membership:ALL){nodes{databaseId login} pageInfo{hasNextPage endCursor}}}}}`

func (c *Client) TeamMember(ctx context.Context, team RequestedTeam, actor Identity) (bool, error) {
	if team.ID <= 0 || team.NodeID == "" || actor.ID <= 0 || actor.Login == "" {
		return false, errors.New("invalid requested-team membership scope")
	}
	variables := map[string]any{"id": team.NodeID, "login": actor.Login, "after": nil}
	cursors, seen := map[string]bool{}, map[int64]bool{}
	found := false
	for page := 0; page < 200; page++ {
		var data struct {
			Node *struct {
				ID         string `json:"id"`
				DatabaseID int64  `json:"databaseId"`
				Members    struct {
					Nodes *[]struct {
						ID    int64  `json:"databaseId"`
						Login string `json:"login"`
					} `json:"nodes"`
					PageInfo pageInfo `json:"pageInfo"`
				} `json:"members"`
			} `json:"node"`
		}
		if err := c.query(ctx, teamMembersQuery, variables, &data); err != nil {
			return false, err
		}
		node := data.Node
		if node == nil || node.ID != team.NodeID || node.DatabaseID != team.ID || node.Members.Nodes == nil {
			return false, errors.New("requested-team membership scope or completeness changed")
		}
		for _, member := range *node.Members.Nodes {
			if member.ID <= 0 || member.Login == "" || seen[member.ID] {
				return false, errors.New("team membership identity is incomplete or repeated")
			}
			seen[member.ID] = true
			if member.ID == actor.ID {
				if !strings.EqualFold(member.Login, actor.Login) {
					return false, errors.New("team member account identity changed")
				}
				found = true
			}
		}
		cursor, more, err := advance(node.Members.PageInfo, cursors)
		if err != nil {
			return false, err
		}
		if !more {
			return found, nil
		}
		variables["after"] = cursor
	}
	return false, errors.New("team membership pagination exceeded its explicit bound")
}

const requestEventsQuery = `query ReviewdRequestEvents($owner:String!,$name:String!,$number:Int!,$after:String){repository(owner:$owner,name:$name){pullRequest(number:$number){databaseId number timelineItems(first:100,after:$after,itemTypes:[REVIEW_REQUESTED_EVENT]){nodes{... on ReviewRequestedEvent{id createdAt requestedReviewer{__typename ... on User{databaseId login} ... on Team{databaseId slug}}}} pageInfo{hasNextPage endCursor}}}}}`

// RequestEvent is an immutable historical fact, independent of current membership.
// A deleted historical reviewer retains the event ID with empty reviewer fields.
type RequestEvent struct {
	ID           string
	ReviewerKind string
	ReviewerID   int64
}

// RequestEvents returns the complete observed history, not an applicability-filtered view.
func (c *Client) RequestEvents(ctx context.Context, pull Pull) ([]RequestEvent, error) {
	canonical, _, err := c.repositoryScope(pull.Repository)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(canonical, "/")
	variables := map[string]any{"owner": parts[1], "name": parts[2], "number": pull.Number, "after": nil}
	cursors, seen := map[string]bool{}, map[string]bool{}
	result := []RequestEvent{}
	for page := 0; page < 200; page++ {
		var data struct {
			Repository struct {
				Pull *struct {
					ID     int64      `json:"databaseId"`
					Number int        `json:"number"`
					Events connection `json:"timelineItems"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.query(ctx, requestEventsQuery, variables, &data); err != nil {
			return nil, err
		}
		item := data.Repository.Pull
		if item == nil || item.ID != pull.ID || item.Number != pull.Number || item.Events.Nodes == nil {
			return nil, errors.New("request-event scope or completeness changed")
		}
		for _, raw := range *item.Events.Nodes {
			var event struct {
				ID       string    `json:"id"`
				Created  time.Time `json:"createdAt"`
				Reviewer *struct {
					Kind string `json:"__typename"`
					ID   int64  `json:"databaseId"`
				} `json:"requestedReviewer"`
			}
			if json.Unmarshal(raw, &event) != nil || event.ID == "" || event.Created.IsZero() || seen[event.ID] {
				return nil, errors.New("request-event identity is incomplete or repeated")
			}
			seen[event.ID] = true
			if event.Reviewer == nil {
				result = append(result, RequestEvent{ID: event.ID})
				continue
			}
			if event.Reviewer.ID <= 0 || (event.Reviewer.Kind != "User" && event.Reviewer.Kind != "Team") {
				return nil, errors.New("requested event reviewer identity is unsupported")
			}
			result = append(result, RequestEvent{ID: event.ID, ReviewerKind: event.Reviewer.Kind, ReviewerID: event.Reviewer.ID})
		}
		cursor, more, err := advance(item.Events.PageInfo, cursors)
		if err != nil {
			return nil, err
		}
		if !more {
			sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
			return result, nil
		}
		variables["after"] = cursor
	}
	return nil, errors.New("request-event pagination exceeded its explicit bound")
}
