package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		showHelp()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "set":
		setVersion()
	case "bump":
		if len(os.Args) < 3 {
			fmt.Println("Error: Must specify bump type (major, minor, patch)")
			os.Exit(1)
		}
		if err := bumpVersion(os.Args[2]); err != nil {
			fmt.Printf("❌ Version bump failed: %v\n", err)
			os.Exit(1)
		}
	default:
		showHelp()
		os.Exit(1)
	}
}

func showHelp() {
	fmt.Println("Version management tool")
	fmt.Println("Usage:")
	fmt.Println("  version set        - Set version from git describe")
	fmt.Println("  version bump TYPE  - Bump version (major, minor, patch)")
}

func getVersionFile() string {
	return "apiframework/version.txt"
}

func getCurrentDescribeVersion() (string, error) {
	cmd := exec.Command("git", "describe", "--tags", "--always", "--dirty")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git version: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

func setVersion() {
	version, err := getCurrentDescribeVersion()
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	filePath := getVersionFile()
	if err := os.WriteFile(filePath, []byte(version), 0644); err != nil {
		fmt.Printf("Error writing version file: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Version set to: %s\n", version)
}

// BumpVersion performs a version bump with proper cleanup on failure
func bumpVersion(bumpType string) error {
	// Start with a clean transaction context
	tx := newBumpTransaction()

	// Ensure cleanup happens on error or panic
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("⚠️  Panic during version bump: %v\n", r)
			tx.Rollback()
			os.Exit(1)
		} else if tx.hasError {
			tx.Rollback()
		}
	}()

	// 1. Verify we're in a git repository
	if !isGitRepository() {
		return fmt.Errorf("not in a git repository")
	}

	// 2. Check for uncommitted changes
	if hasUncommittedChanges() {
		return fmt.Errorf("cannot create release with uncommitted changes. Please commit or stash your changes first")
	}

	// 3. Get current version
	currentVersion, err := getCurrentTagVersion()
	if err != nil {
		return fmt.Errorf("failed to get current version: %w", err)
	}
	fmt.Printf("Current version: %s\n", currentVersion)
	tx.currentVersion = currentVersion

	// 4. Calculate new version
	newVersion, err := calculateNewVersion(currentVersion, bumpType)
	if err != nil {
		return fmt.Errorf("failed to calculate new version: %w", err)
	}
	fmt.Printf("New version will be: %s\n", newVersion)
	tx.newVersion = newVersion

	// 5. Update compose file
	if err := updateComposeFile(newVersion); err != nil {
		return fmt.Errorf("failed to update compose file: %w", err)
	}
	tx.composeUpdated = true

	// 6. Update version file
	if err := updateVersionFile(newVersion); err != nil {
		return fmt.Errorf("failed to update version file: %w", err)
	}
	tx.versionFileUpdated = true

	// 6b. Update README install snippet TAG so copy-paste URL stays correct
	if err := updateReadmeTag(newVersion); err != nil {
		return fmt.Errorf("failed to update README TAG: %w", err)
	}
	tx.readmeUpdated = true

	// 7. Commit changes
	if err := commitVersionFile(newVersion); err != nil {
		return fmt.Errorf("failed to commit version changes: %w", err)
	}
	tx.commitCreated = true

	// 8. Create tag
	if err := createTag(newVersion); err != nil {
		return fmt.Errorf("failed to create tag: %w", err)
	}
	tx.tagCreated = true

	// 9. Regenerate docs
	fmt.Println("\n🔄 Regenerating documentation with new version...")
	if err := updateDocsAndAmendCommit(); err != nil {
		return fmt.Errorf("failed to update documentation: %w", err)
	}

	// Success - no rollback needed
	tx.markSuccessful()

	fmt.Printf("\n✅ Release %s created successfully!\n", newVersion)
	fmt.Printf("   Push with: git push && git push origin %s\n", newVersion)
	return nil
}

func updateDocsAndAmendCommit() error {
	// Regenerate OpenAPI spec and Markdown
	cmd := exec.Command("make", "docs-markdown")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to run 'make docs-markdown': %w\nOutput: %s", err, string(output))
	}

	// Add updated docs to index
	cmd = exec.Command("git", "add", "docs/")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to git add docs/: %w\nOutput: %s", err, string(output))
	}

	cmd = exec.Command("git", "commit", "--amend", "--no-edit")
	if output, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(output), "nothing to commit") {
			fmt.Println("   Documentation was already up-to-date.")
			return nil
		}
		return fmt.Errorf("failed to amend commit: %w\nOutput: %s", err, string(output))
	}

	fmt.Println("   Documentation updated and included in the release commit.")
	return nil
}

func isGitRepository() bool {
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	return cmd.Run() == nil
}

// hasUncommittedChanges checks for any changes in the git repository, ignoring the version file.
func hasUncommittedChanges() bool {
	versionFilePath := getVersionFile()

	cmd := exec.Command("git", "status", "--porcelain")
	output, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("Git error checking for uncommitted changes: %s\n", string(output))
		return true // Fail safe
	}

	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine == "" {
			continue
		}

		// If a line indicates a change and it's NOT the version file,
		// then we have uncommitted changes that need to be addressed.
		if !strings.HasSuffix(trimmedLine, versionFilePath) {
			return true
		}
	}

	return false
}

// getCurrentTagVersion fetches the latest semantic version tag from the repository.
func getCurrentTagVersion() (string, error) {
	// Fetch all tags from the remote repository
	exec.Command("git", "fetch", "--tags").Run() // Ignore errors for offline scenarios

	// Get the latest tag by sorting them using version semantics
	cmd := exec.Command("git", "tag", "--sort=-v:refname")
	output, err := cmd.CombinedOutput()
	if err != nil || len(output) == 0 {
		return "v0.1.0", nil
	}

	tags := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(tags) == 0 || tags[0] == "" {
		return "v0.1.0", nil
	}

	return tags[0], nil
}

// calculateNewVersion increments a semantic version string based on the bump type.
func calculateNewVersion(currentVersion, bumpType string) (string, error) {
	// Remove 'v' prefix for parsing
	if !strings.HasPrefix(currentVersion, "v") {
		return "", fmt.Errorf("invalid version format: missing 'v' prefix in '%s'", currentVersion)
	}
	tag := strings.TrimPrefix(currentVersion, "v")

	parts := strings.Split(tag, ".")
	if len(parts) != 3 {
		return "v0.1.0", nil
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return "", fmt.Errorf("invalid major version in '%s': %w", tag, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", fmt.Errorf("invalid minor version in '%s': %w", tag, err)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", fmt.Errorf("invalid patch version in '%s': %w", tag, err)
	}

	// Bump version based on type
	switch bumpType {
	case "major":
		major++
		minor = 0
		patch = 0
	case "minor":
		minor++
		patch = 0
	case "patch":
		patch++
	default:
		return "", fmt.Errorf("unknown bump type '%s'. Use 'major', 'minor', or 'patch'", bumpType)
	}

	return fmt.Sprintf("v%d.%d.%d", major, minor, patch), nil
}

func updateVersionFile(newVersion string) error {
	fmt.Printf("📝 Updating version file to %s...\n", newVersion)
	return os.WriteFile(getVersionFile(), []byte(newVersion), 0644)
}

func commitVersionFile(newVersion string) error {
	fmt.Println("📦 Committing version, compose, and README...")

	// Add version file, compose, and README (install snippet TAG) to the commit
	cmd := exec.Command("git", "add", getVersionFile(), "compose.yaml", "README.md")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add version and compose files: %w\nOutput: %s", err, string(output))
	}

	// Commit the change
	cmd = exec.Command("git", "commit", "-m", fmt.Sprintf("chore: release %s", newVersion))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to commit version file: %w\nOutput: %s", err, string(output))
	}

	return nil
}

func createTag(newVersion string) error {
	fmt.Printf("🔖 Creating tag %s...\n", newVersion)

	// Create an annotated tag pointing to the release commit
	cmd := exec.Command("git", "tag", "-a", newVersion, "-m", fmt.Sprintf("Release %s", newVersion))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create tag: %w\nOutput: %s", err, string(output))
	}

	return nil
}

func updateComposeFile(newVersion string) error {
	composePath := "compose.yaml"

	// Check if compose file exists
	if _, err := os.Stat(composePath); os.IsNotExist(err) {
		fmt.Println("   ⚠️  compose.yaml not found, skipping compose update")
		return nil
	}

	fmt.Printf("   🔄 Updating %s to use version %s...\n", composePath, newVersion)

	// Read the compose file
	content, err := os.ReadFile(composePath)
	if err != nil {
		return fmt.Errorf("failed to read compose file: %w", err)
	}

	// Replace the runtime image tag
	re := regexp.MustCompile(`image: ghcr\.io/contenox/runtime-api:[^\s]+`)
	updatedContent := re.ReplaceAllString(string(content), "image: ghcr.io/contenox/runtime-api:"+newVersion)

	// Write the updated content
	if err := os.WriteFile(composePath, []byte(updatedContent), 0644); err != nil {
		return fmt.Errorf("failed to write compose file: %w", err)
	}

	return nil
}

func updateReadmeTag(newVersion string) error {
	readmePath := "README.md"
	content, err := os.ReadFile(readmePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", readmePath, err)
	}
	re := regexp.MustCompile(`TAG=v\d+\.\d+\.\d+`)
	if !re.Match(content) {
		return fmt.Errorf("README.md has no TAG=vX.Y.Z line for install snippet")
	}
	updated := re.ReplaceAll(content, []byte("TAG="+newVersion))
	if bytes.Equal(updated, content) {
		return nil
	}
	if err := os.WriteFile(readmePath, updated, 0644); err != nil {
		return fmt.Errorf("write %s: %w", readmePath, err)
	}
	fmt.Printf("   📝 Updated README install snippet to %s\n", newVersion)
	return nil
}

// BumpTransaction represents a version bump operation with state tracking for proper cleanup
type bumpTransaction struct {
	currentVersion      string
	newVersion          string
	composeUpdated      bool
	versionFileUpdated  bool
	readmeUpdated       bool
	commitCreated       bool
	tagCreated          bool
	hasError            bool
	successful          bool
	previousComposePath string
	previousVersionPath string
}

// newBumpTransaction creates a new transaction context
func newBumpTransaction() *bumpTransaction {
	return &bumpTransaction{
		hasError:   true, // Assume failure until proven otherwise
		successful: false,
	}
}

// markSuccessful marks the transaction as successful
func (tx *bumpTransaction) markSuccessful() {
	tx.successful = true
	tx.hasError = false
}

// Rollback attempts to revert all changes made during the transaction
func (tx *bumpTransaction) Rollback() {
	fmt.Println("\n🔄 Rolling back version bump...")

	// If we successfully created a tag, remove it
	if tx.tagCreated {
		fmt.Printf("   Removing tag %s...\n", tx.newVersion)
		exec.Command("git", "tag", "-d", tx.newVersion).Run()
	}

	// If we successfully created a commit, reset it
	if tx.commitCreated {
		fmt.Println("   Resetting last commit...")
		exec.Command("git", "reset", "HEAD~1").Run()
	}

	// If we updated the version file, revert it
	if tx.versionFileUpdated {
		fmt.Printf("   Restoring version file to %s...\n", tx.currentVersion)
		os.WriteFile(getVersionFile(), []byte(tx.currentVersion), 0644)
	}

	// If we updated the compose file, revert it
	if tx.composeUpdated {
		fmt.Println("   Restoring compose file...")
		composePath := "compose.yaml"

		// Read the compose file
		content, err := os.ReadFile(composePath)
		if err != nil {
			fmt.Printf("   Failed to read compose file: %v\n", err)
			return
		}

		// Replace the runtime image tag back to current version
		re := regexp.MustCompile(`image: ghcr\.io/contenox/runtime-api:[^\s]+`)
		updatedContent := re.ReplaceAllString(string(content), "image: ghcr.io/contenox/runtime-api:"+tx.currentVersion)

		// Write the restored content
		if err := os.WriteFile(composePath, []byte(updatedContent), 0644); err != nil {
			fmt.Printf("   Failed to restore compose file: %v\n", err)
		}
	}

	// If we updated the README TAG line, revert it
	if tx.readmeUpdated {
		fmt.Println("   Restoring README TAG...")
		if err := updateReadmeTag(tx.currentVersion); err != nil {
			fmt.Printf("   Failed to restore README: %v\n", err)
		}
	}

	fmt.Println("   Rollback completed.")
}
