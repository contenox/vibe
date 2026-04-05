package vfsstore

import (
	"context"

	"github.com/contenox/contenox/libdbexec"
)

// InitSchema creates the VFS tables and indexes if they do not already exist.
func InitSchema(ctx context.Context, exec libdbexec.Exec) error {
	_, err := exec.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS vfs_files (
		    id VARCHAR(255) PRIMARY KEY,
		    type VARCHAR(512) NOT NULL,
		    meta JSONB NOT NULL,
		    blobs_id VARCHAR(255),
		    is_folder BOOLEAN DEFAULT FALSE,
		    created_at TIMESTAMP NOT NULL,
		    updated_at TIMESTAMP NOT NULL
		);
	`)
	if err != nil {
		return err
	}

	_, err = exec.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS vfs_filestree (
		    id VARCHAR(255) PRIMARY KEY,
		    parent_id VARCHAR(255),
		    name VARCHAR(1024) NOT NULL,
		    created_at TIMESTAMP NOT NULL,
		    updated_at TIMESTAMP NOT NULL,
		    UNIQUE (parent_id, name)
		);
	`)
	if err != nil {
		return err
	}

	_, err = exec.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS vfs_blobs (
		    id VARCHAR(255) PRIMARY KEY,
		    meta JSONB NOT NULL,
		    data bytea NOT NULL,
		    created_at TIMESTAMP NOT NULL,
		    updated_at TIMESTAMP NOT NULL
		);
	`)
	if err != nil {
		return err
	}

	_, err = exec.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_vfs_filestree_parent_id ON vfs_filestree USING hash(parent_id);`)
	if err != nil {
		return err
	}

	_, err = exec.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_vfs_filestree_list ON vfs_filestree (name, parent_id);`)
	if err != nil {
		return err
	}

	// vfs_filestree.id → vfs_files.id   (CASCADE: delete file record removes tree entry)
	// vfs_files.blobs_id → vfs_blobs.id (SET NULL: deleting a blob marks file as blob-less)
	_, err = exec.ExecContext(ctx, `
		DO $$ BEGIN
		    IF NOT EXISTS (
		        SELECT 1 FROM pg_constraint WHERE conname = 'fk_tree_file'
		    ) THEN
		        ALTER TABLE vfs_filestree
		            ADD CONSTRAINT fk_tree_file
		            FOREIGN KEY (id) REFERENCES vfs_files(id) ON DELETE CASCADE;
		    END IF;
		    IF NOT EXISTS (
		        SELECT 1 FROM pg_constraint WHERE conname = 'fk_file_blob'
		    ) THEN
		        ALTER TABLE vfs_files
		            ADD CONSTRAINT fk_file_blob
		            FOREIGN KEY (blobs_id) REFERENCES vfs_blobs(id) ON DELETE SET NULL;
		    END IF;
		END $$;
	`)
	return err
}
