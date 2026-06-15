#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# check-doc-stubs.sh - fail on tautological doc-comment stubs
#
# A doc generator / IDE quick-fix sometimes emits placeholder comments that
# just respell an identifier as lowercase words ("// AddAll add all."). They
# add no information and violate the project's WHY-not-WHAT comment rule, so
# CI rejects them. Replace each with a real doc comment, or remove it.
# ---------------------------------------------------------------------------
set -euo pipefail

hits=$(
	git ls-files '*.go' | grep -v '_test\.go$' | while IFS= read -r f; do
		perl -ne '
			if (m{^\s*// ([A-Za-z][A-Za-z0-9]*) (.+)\.\s*$}) {
				my ($id, $rest) = ($1, $2);
				(my $words = $id) =~ s/([a-z0-9])([A-Z])/$1 $2/g;
				print "$ARGV:$.: $_" if lc($rest) eq lc($words);
			}
		' "$f"
	done
)

if [ -n "$hits" ]; then
	echo "doc-comment stubs found (the comment just respells the identifier):" >&2
	echo "$hits" >&2
	echo "Replace each with a real doc comment, or remove it." >&2
	exit 1
fi
