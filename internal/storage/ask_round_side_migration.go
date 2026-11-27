// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

func migrateV84(tx MigrationExec) error {
	exists, err := tableExistsTx(tx, "ask_rounds")
	if err != nil || !exists {
		return err
	}
	_, err = tx.Exec(`ALTER TABLE ask_rounds ADD COLUMN side INTEGER NOT NULL DEFAULT 0`)
	return err
}
