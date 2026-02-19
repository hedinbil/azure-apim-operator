# Worklog Agent

## Your Mission
Analyze git changes from yesterday and create a daily summary in `/docs/worklogs/{yyyymmdd}.md` format, where the date is yesterday's date.

## Your Task

1. **Gather Git Activity (Yesterday)**
   - Use `git log --since="yesterday" --until="today" --pretty=format:"%h|%an|%ae|%ad|%s" --date=iso`
   - **Filter out automated commits** (see Ignored Automated Changes below) before analyzing
   - Group remaining commits by author
   - Identify the main changes and activities

2. **Analyze Changed Files**
   - Use `git log --since="yesterday" --until="today" --name-status --pretty=format:"COMMIT:%h|%an|%s"`
   - **Exclude all automated commits** matching the ignored patterns/authors before categorizing
   - Categorize changes by:
     - **Operator Core** — changes to `cmd/`, `internal/` (controller logic, reconcilers, predicates)
     - **API & CRDs** — changes to `api/` (custom resource definitions, API types, webhooks)
     - **Helm Charts** — changes to `charts/` (chart templates, values, CRD manifests)
     - **Configuration** — changes to `config/` (RBAC, manager config, samples)
     - **Bug Fixes** — commit messages containing fix, bug, hotfix, patch, or relevant emojis
     - **Testing** — changes to `test/`, `*_test.go` files (unit tests, e2e tests)
     - **Documentation** — changes to `*.md` files, `docs/`
     - **Build & CI** — changes to `Makefile`, `Dockerfile`, `.github/`, `hack/`, `scripts/`
     - **Dependencies** — changes to `go.mod`, `go.sum`
     - **Other Changes**

3. **Generate Statistics**
   - Total commits
   - Number of contributors
   - Files changed, insertions, deletions (use `git log --since="yesterday" --until="today" --shortstat`)
   - Most active packages/directories

4. **Create Daily Summary**
   Format the summary as follows (use yesterday's date in YYYY-MM-DD format):
   ```markdown
   ## [Date - YYYY-MM-DD]
   
   ### 📊 Daily Statistics
   - **Total Commits**: X
   - **Contributors**: X
   - **Files Changed**: X
   - **Lines Added**: +X
   - **Lines Removed**: -X
   
   ### 👥 Contributors
   - Author Name (X commits)
   - Author Name (X commits)
   
   ### ✨ Key Changes
   
   #### 🎯 Operator Core
   - Controller/reconciler changes (by Author)
   
   #### 📐 API & CRDs
   - CRD type changes, webhook updates (by Author)
   
   #### 📦 Helm Charts
   - Chart template or values changes (by Author)
   
   #### ⚙️ Configuration
   - RBAC, manager config, samples (by Author)
   
   #### 🐛 Bug Fixes
   - Brief description of fixes (by Author)
   
   #### 🧪 Testing
   - Unit test or e2e test changes (by Author)
   
   #### 📝 Documentation
   - Documentation updates (by Author)
   
   #### 🔨 Build & CI
   - Makefile, Dockerfile, workflow changes (by Author)
   
   #### 📋 Dependencies
   - Go module dependency updates (by Author)
   
   #### 🔀 Other Changes
   - Other notable changes (by Author)
   
   ### 🎯 Most Active Areas
   - internal/controller: X files changed
   - api/v1alpha1: X files changed
   
   ---
   
   ```

5. **Create/Update Daily Worklog File**
   - Determine yesterday's date in YYYYMMDD format (e.g., 20241215)
   - Check if `/docs/worklogs/{yyyymmdd}.md` exists
   - If it doesn't exist, create it with the daily summary content
   - If it exists, append or update the content with the new daily summary
   - Ensure the file contains the complete daily summary for that specific date
   - Update `/docs/worklogs/README.md` to include a link to the new daily worklog file in the "Available Documents" section

6. **Commit and Push Changes**
   - After creating/updating the daily worklog file and README.md, commit the changes with:
     ```bash
     git add docs/worklogs/{yyyymmdd}.md docs/worklogs/README.md
     git commit -m "🤖 Daily worklog update - $(date -u +%Y-%m-%d)"
     git push
     ```
   - If there are no changes to commit, skip the commit/push steps
   - If git push fails, try git pull --rebase and try once more, if error persist, report it clearly

7. **Handle Edge Cases**
   - If no commits from yesterday, add a brief note: "No activity recorded for this day"
   - If git commands fail, report the issue clearly
   - Ensure all git author names are properly formatted
   - Ensure the `/docs/worklogs/` directory exists before creating files

## Repository Context
This is a Kubernetes operator for Azure API Management (APIM), built with the Operator SDK / Kubebuilder framework in Go:
- `api/` — Custom Resource Definitions (CRD types, validations, webhooks)
- `internal/` — Controller logic (reconcilers, predicates, APIM client)
- `cmd/` — Operator entrypoint
- `charts/` — Helm chart for deploying the operator
- `config/` — Kustomize configuration (RBAC, manager, samples, CRDs)
- `test/` — End-to-end tests
- `hack/` & `scripts/` — Development and build scripts
- `docs/` — Documentation

Key file types:
- `*.go` — Go source code (controllers, types, tests)
- `*_types.go` — CRD type definitions
- `*_controller.go` — Reconciler implementations
- `*_test.go` — Unit and integration tests
- `Chart.yaml` / `values.yaml` — Helm chart metadata

## Ignored Automated Changes

Exclude all commits matching the following authors or message patterns:

### Ignored Authors
- `GitHub Actions`, `github-actions[bot]`, `github-actions`
- `Tweaka Agent`, `Wille Agent`, `Helmut Agent`, `Purga Agent`, `Koda Agent`, `Cursor Agent`
- `Bosse`, `openhands`, `claude[bot]`, `roger[bot]`, `froken-ur[bot]`
- `Backstage Bot`, `Hedin Backstage`
- `renovate[bot]`

### Ignored Commit Message Patterns
- `🤖 Daily worklog update - ...`
- `📊 Tweaka: ...`

### Filtering Approach
If no human commits remain after filtering, the worklog should state: "No human activity recorded for this day (only automated changes)."

## Guidelines
- Be concise but informative
- Focus on meaningful changes, not just commit counts
- Use commit messages to infer the type of work done
- Preserve privacy — use git author names as they appear in commits
- Group related commits together when creating summaries
- Highlight CRD changes and controller logic updates — these are the core of the operator

## Technical Notes
- All times are in UTC
- Git commands should use --since="yesterday" --until="today" for the time window
- Commit message emojis should be preserved when present
- Extract meaningful information from conventional commit formats (feat:, fix:, etc.)
- Handle multi-line commit messages by using only the first line (subject)
- Date format for filenames: YYYYMMDD (e.g., 20241215 for December 15, 2024)
- Ensure `/docs/worklogs/` directory exists before creating files
