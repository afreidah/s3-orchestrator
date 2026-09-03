{{- /*
  Search index, overriding the theme's copy to leave out the generated Go API
  reference and the empty taxonomy pages.

  The godoc tree is roughly three quarters of the site's pages, so indexing it
  puts most of the index's weight - and this file loads on every page - into
  generated API text, which then buries the hand-written documentation in the
  results. The pages stay in the menu and remain reachable; they are simply not
  searched.
*/ -}}
{{- $skip := slice "/godoc/" "/categories/" "/tags/" }}
{{- $pages := slice }}
{{- range site.Pages }}
  {{- $page := . }}
  {{- $excluded := false }}
  {{- range $skip }}
    {{- if hasPrefix $page.RelPermalink . }}{{ $excluded = true }}{{ end }}
  {{- end }}
  {{- if or $excluded (partial "_relearn/pageIsSpecial.gotmpl" .) }}
  {{- else if and .Title .RelPermalink (or (ne site.Params.disableSearchHiddenPages true) (not (partialCached "_relearn/pageIsHiddenSelfOrAncestor.gotmpl" (dict "page" . "to" site.Home) .Path site.Home.Path) ) ) }}
    {{- $tags := slice }}
    {{- range .GetTerms "tags" }}
      {{- $tags = $tags | append (partial "title.gotmpl" (dict "page" .Page "linkTitle" true) | plainify) }}
    {{- end }}
    {{- $pages = $pages | append (dict
      "uri" (partial "permalink.gotmpl" (dict "to" .))
      "title" (partial "title.gotmpl" (dict "page" .) | plainify)
      "tags" $tags
      "breadcrumb" (trim (partial "breadcrumbs.html" (dict "page" . "dirOnly" true) | plainify | htmlUnescape) "\n\r\t ")
      "description" (trim (or .Description .Summary | plainify | htmlUnescape) "\n\r\t " )
      "content" (trim (.Plain | htmlUnescape) "\n\r\t ")
    ) }}
  {{- end }}
{{- end -}}
var relearn_searchindex = {{ $pages | jsonify (dict "indent" "  ") }}
