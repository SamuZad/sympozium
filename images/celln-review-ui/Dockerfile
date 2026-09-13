# Build the embedded review UI with VITE_CELLN_REVIEW=true, then compile
# CGO_ENABLED=0 go build -trimpath -o <context>/celln-review-ui ./cmd/celln-review-ui.
# This deliberately contains no shell, provider keys or execution authority.
FROM scratch
COPY celln-review-ui /celln-review-ui
USER 65532:65532
EXPOSE 8443
ENTRYPOINT ["/celln-review-ui"]
