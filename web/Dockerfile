# -------------------------------------------------------------------------------
# Documentation Site Container
#
# Author: Alex Freidah
#
# Multi-stage build that produces the static documentation site.
# Stage 1 (godoc): runs gomarkdoc to render Go API references into Hugo
# content. Stage 2 (hugo): copies the rendered markdown plus the rest of
# web/content/ and runs Hugo to produce the final static site. Stage 3
# serves the site via nginx so the same image works as both a build
# artifact and a deployable container.
# -------------------------------------------------------------------------------

FROM --platform=$BUILDPLATFORM golang:1.27.0 AS godoc
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN go install github.com/princjef/gomarkdoc/cmd/gomarkdoc@latest
# Discover packages the same way `make web-godoc` does so the published site
# never silently drops a package. The exclude pattern mirrors GODOC_EXCLUDES
# in the Makefile - keep the two in sync.
RUN mkdir -p /godoc && \
    go list -f '{{.ImportPath}}' ./internal/... \
        | sed 's|^github.com/afreidah/s3-orchestrator/internal/||' \
        | grep -vE '^(integration|testutil|testutil/.*|backend/backendtest|store/postgres/sqlc|store/postgres/migrations|.*/testdata|.*/testdata/.*)$' \
        | sort \
        | while read pkg; do \
            name=$(basename "$pkg"); \
            printf -- '---\ntitle: "%s"\n---\n\n' "$name" > /godoc/$name.md; \
            gomarkdoc ./internal/$pkg >> /godoc/$name.md; \
            sed -i '/^# '"$name"'$/d' /godoc/$name.md; \
        done

FROM --platform=$BUILDPLATFORM hugomods/hugo:latest AS build
WORKDIR /src/web
COPY web/ /src/web/
COPY docs/ /src/docs/
COPY README.md /src/README.md
COPY CHANGELOG.md /src/CHANGELOG.md
COPY --from=godoc /godoc/ /src/web/content/godoc/
# Restore the hand-maintained index page overwritten by the godoc stage
COPY web/content/godoc/_index.md /src/web/content/godoc/_index.md
RUN hugo --minify

FROM nginx:alpine
# APK_CACHEBUST changes per build (CI passes the commit SHA) so this layer is
# never served stale from the build cache: apk upgrade re-runs against the
# current Alpine index and picks up base-image security patches even when the
# nginx:alpine digest is unchanged.
ARG APK_CACHEBUST=0
RUN APK_CACHEBUST="$APK_CACHEBUST" apk upgrade --no-cache
COPY --from=build /src/web/public /usr/share/nginx/html
EXPOSE 80
