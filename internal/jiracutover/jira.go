package jiracutover

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

type JiraClient struct {
	Email, Token string
	HTTP         *http.Client
}

func (c JiraClient) Head(ctx context.Context, site, issue string) (head postgres.JiraHistoryWatermark, found bool, err error) {
	found, err = c.maxID(ctx, site, "/rest/api/3/issue/"+url.PathEscape(issue)+"/changelog", "values", &head.ChangelogID)
	if err != nil || !found {
		return
	}
	_, err = c.maxID(ctx, site, "/rest/api/3/issue/"+url.PathEscape(issue)+"/comment", "comments", &head.CommentID)
	return
}

func (c JiraClient) maxID(ctx context.Context, site, path, field string, out *string) (bool, error) {
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	for start := 0; ; {
		u := "https://" + site + path + fmt.Sprintf("?startAt=%d&maxResults=100", start)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.SetBasicAuth(c.Email, c.Token)
		req.Header.Set("Accept", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return false, err
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			return false, nil
		}
		if resp.StatusCode/100 != 2 {
			resp.Body.Close()
			return false, fmt.Errorf("Jira %s returned %s", path, resp.Status)
		}
		var page struct {
			Total  int
			Values []struct {
				ID string `json:"id"`
			} `json:"values"`
			Comments []struct {
				ID string `json:"id"`
			} `json:"comments"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return false, err
		}
		rows := page.Values
		if field == "comments" {
			rows = page.Comments
		}
		for _, row := range rows {
			if greaterDecimal(row.ID, *out) {
				*out = row.ID
			}
		}
		start += len(rows)
		if start >= page.Total || len(rows) == 0 {
			return true, nil
		}
	}
}

func greaterDecimal(a, b string) bool {
	av, ok := new(big.Int).SetString(a, 10)
	if !ok {
		return false
	}
	if b == "" {
		return true
	}
	bv, ok := new(big.Int).SetString(b, 10)
	return ok && av.Cmp(bv) > 0
}
