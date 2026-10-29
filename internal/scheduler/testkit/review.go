package testkit

const ApprovedReview = `printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"schema_version":4,"criteria":[],"human_action":null,"prior_findings":[],"status":"approved","summary":"Safe <script>review</script>","findings":[{"id":"R1-F1","severity":"warning","title":"Optional","details":"Later","file":null,"line":null}]}}'`

func ReviewScript(review string) string {
	return `case "$*" in *review-*)
` + review + `
;; *)
` + SuccessfulScript + `
;; esac`
}
