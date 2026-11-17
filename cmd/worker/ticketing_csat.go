package main

import (
	"strconv"
	"strings"
)

// csatScoreLabels name the five scores on the survey email, worst first.
var csatScoreLabels = [5]string{"Very poor", "Poor", "Okay", "Good", "Excellent"}

// csatScoreLink is one score button on the survey email: a click opens the survey with that score
// already picked.
type csatScoreLink struct {
	Score int
	Label string
	URL   string
}

// csatScoreURL is the survey link with score=n added, so the survey page opens with that score
// picked. ticketing-api's csat_url already carries tenant and token, so the score goes after "&".
func csatScoreURL(base string, score int) string {
	sep := "&"
	if !strings.Contains(base, "?") {
		sep = "?"
	} else if strings.HasSuffix(base, "?") || strings.HasSuffix(base, "&") {
		sep = ""
	}
	return base + sep + "score=" + strconv.Itoa(score)
}

// csatScoreLinks are the five score links for a survey URL, 1 to 5.
func csatScoreLinks(base string) []csatScoreLink {
	links := make([]csatScoreLink, 0, len(csatScoreLabels))
	for i, label := range csatScoreLabels {
		links = append(links, csatScoreLink{Score: i + 1, Label: label, URL: csatScoreURL(base, i+1)})
	}
	return links
}

// ticketCSATMapping is the satisfaction survey sent to the requester after a ticket is resolved
// (ticketing.ticket.csat_requested). Same route as the other requester notices: their email,
// Reply-To the tenant's support address, threaded onto the ticket's email thread with the ticket
// tag in the subject, nothing when they have no email. It goes by email only: ticketing-api
// raises the event only with survey_channel email, and ticket mail is email only by owner decision.
var ticketCSATMapping = ticketingNotificationMapping{
	TemplateID:   "ticketing/ticket_csat_survey",
	EmailSubject: "How did we do with your support request?",
	Audience:     audienceRequester,
	Skip: func(p map[string]any) bool {
		if ch := ticketText(p, "survey_channel"); ch != "" && ch != "email" {
			return true
		}
		return ticketText(p, "csat_url") == ""
	},
	DataBuilder: func(p map[string]any, links ticketLinks) map[string]any {
		d := requesterBase(p, links)
		survey := ticketText(p, "csat_url")
		d["csat_url"] = survey
		d["score_links"] = csatScoreLinks(survey)
		d["expires_at"] = formatEventTime(ticketText(p, "csat_expires_at"))
		return d
	},
}
