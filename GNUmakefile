.PHONY: generate

# Regenerate docs/ from provider schemas and examples/. Tools live in their own
# module so their dependencies stay out of the provider go.mod.
generate:
	cd tools; go generate ./...
