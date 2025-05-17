HH_POSTGRES_CONNECT := $(HH_POSTGRES_CONNECT)

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'


push: ##  git push into productio
	git add .
	git commit -m "auto front"
	-git push origin main
	git push cursor main
	echo "https://github.com/reactima/cursor"
	echo "https://gisowl.com/"

save: ## save
	git add .
	git commit -m "auto front"
	git push cursor main
	echo "https://github.com/reactima/cursor"

.DEFAULT_GOAL := help
