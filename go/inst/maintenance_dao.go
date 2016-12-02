/*
   Copyright 2014 Outbrain Inc.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package inst

import (
	"fmt"

	"github.com/outbrain/golib/log"
	"github.com/outbrain/golib/sqlutils"
	"github.com/github/orchestrator/go/config"
	"github.com/github/orchestrator/go/db"
	"github.com/github/orchestrator/go/process"
)

// ReadActiveMaintenance returns the list of currently active maintenance entries
func ReadActiveMaintenance() ([]Maintenance, error) {
	res := []Maintenance{}
	query := `
		select
			database_instance_maintenance_id,
			hostname,
			port,
			begin_timestamp,
			timestampdiff(second, begin_timestamp, now()) as seconds_elapsed,
			maintenance_active,
			owner,
			reason
		from
			database_instance_maintenance
		where
			maintenance_active = 1
		order by
			database_instance_maintenance_id
		`
	err := db.QueryOrchestratorRowsMap(query, func(m sqlutils.RowMap) error {
		maintenance := Maintenance{}
		maintenance.MaintenanceId = m.GetUint("database_instance_maintenance_id")
		maintenance.Key.Hostname = m.GetString("hostname")
		maintenance.Key.Port = m.GetInt("port")
		maintenance.BeginTimestamp = m.GetString("begin_timestamp")
		maintenance.SecondsElapsed = m.GetUint("seconds_elapsed")
		maintenance.IsActive = m.GetBool("maintenance_active")
		maintenance.Owner = m.GetString("owner")
		maintenance.Reason = m.GetString("reason")

		res = append(res, maintenance)
		return nil
	})

	if err != nil {
		log.Errore(err)
	}
	return res, err

}

// BeginBoundedMaintenance will make new maintenance entry for given instanceKey.
func BeginBoundedMaintenance(instanceKey *InstanceKey, owner string, reason string, durationSeconds uint, explicitlyBounded bool) (int64, error) {
	var maintenanceToken int64 = 0
	if durationSeconds == 0 {
		durationSeconds = config.Config.MaintenanceExpireMinutes * 60
	}
	res, err := db.ExecOrchestrator(`
			insert ignore
				into database_instance_maintenance (
					hostname, port, maintenance_active, begin_timestamp, end_timestamp, owner, reason,
					processing_node_hostname, processing_node_token, explicitly_bounded
				) VALUES (
					?, ?, 1, NOW(), NOW() + INTERVAL ? SECOND, ?, ?,
					?, ?, ?
				)
			`,
		instanceKey.Hostname,
		instanceKey.Port,
		durationSeconds,
		owner,
		reason,
		process.ThisHostname,
		process.ProcessToken.Hash,
		explicitlyBounded,
	)
	if err != nil {
		return maintenanceToken, log.Errore(err)
	}

	if affected, _ := res.RowsAffected(); affected == 0 {
		err = fmt.Errorf("Cannot begin maintenance for instance: %+v; maintenance reason: %+v", instanceKey, reason)
	} else {
		// success
		maintenanceToken, _ = res.LastInsertId()
		AuditOperation("begin-maintenance", instanceKey, fmt.Sprintf("maintenanceToken: %d, owner: %s, reason: %s", maintenanceToken, owner, reason))
	}
	return maintenanceToken, err
}

// BeginMaintenance will make new maintenance entry for given instanceKey. Maintenance time is unbounded
func BeginMaintenance(instanceKey *InstanceKey, owner string, reason string) (int64, error) {
	return BeginBoundedMaintenance(instanceKey, owner, reason, 0, false)
}

// EndMaintenanceByInstanceKey will terminate an active maintenance using given instanceKey as hint
func EndMaintenanceByInstanceKey(instanceKey *InstanceKey) error {
	res, err := db.ExecOrchestrator(`
			update
				database_instance_maintenance
			set
				maintenance_active = NULL,
				end_timestamp = NOW()
			where
				hostname = ?
				and port = ?
				and maintenance_active = 1
			`,
		instanceKey.Hostname,
		instanceKey.Port,
	)
	if err != nil {
		return log.Errore(err)
	}

	if affected, _ := res.RowsAffected(); affected == 0 {
		err = fmt.Errorf("Instance is not in maintenance mode: %+v", instanceKey)
	} else {
		// success
		AuditOperation("end-maintenance", instanceKey, "")
	}
	return err
}

// ReadMaintenanceInstanceKey will return the instanceKey for active maintenance by maintenanceToken
func ReadMaintenanceInstanceKey(maintenanceToken int64) (*InstanceKey, error) {
	var res *InstanceKey
	query := `
		select
			hostname, port
		from
			database_instance_maintenance
		where
			database_instance_maintenance_id = ?
			`

	err := db.QueryOrchestrator(query, sqlutils.Args(maintenanceToken), func(m sqlutils.RowMap) error {
		instanceKey, merr := NewInstanceKeyFromStrings(m.GetString("hostname"), m.GetString("port"))
		if merr != nil {
			return merr
		}

		res = instanceKey
		return nil
	})

	if err != nil {
		log.Errore(err)
	}
	return res, err
}

// EndMaintenance will terminate an active maintenance via maintenanceToken
func EndMaintenance(maintenanceToken int64) error {
	res, err := db.ExecOrchestrator(`
			update
				database_instance_maintenance
			set
				maintenance_active = NULL,
				end_timestamp = NOW()
			where
				database_instance_maintenance_id = ?
			`,
		maintenanceToken,
	)
	if err != nil {
		return log.Errore(err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		err = fmt.Errorf("Instance is not in maintenance mode; token = %+v", maintenanceToken)
	} else {
		// success
		instanceKey, _ := ReadMaintenanceInstanceKey(maintenanceToken)
		AuditOperation("end-maintenance", instanceKey, fmt.Sprintf("maintenanceToken: %d", maintenanceToken))
	}
	return err
}

// ExpireMaintenance will remove the maintenance flag on old maintenances and on bounded maintenances
func ExpireMaintenance() error {
	{
		res, err := db.ExecOrchestrator(`
			delete from
				database_instance_maintenance
			where
				maintenance_active is null
				and end_timestamp < NOW() - INTERVAL ? DAY
			`,
			config.Config.MaintenancePurgeDays,
		)
		if err != nil {
			return log.Errore(err)
		}
		if rowsAffected, _ := res.RowsAffected(); rowsAffected > 0 {
			AuditOperation("expire-maintenance", nil, fmt.Sprintf("Purged historical entries: %d", rowsAffected))
		}
	}
	{
		res, err := db.ExecOrchestrator(`
			update
				database_instance_maintenance
			set
				maintenance_active = NULL
			where
				maintenance_active = 1
				and end_timestamp < NOW()
			`,
		)
		if err != nil {
			return log.Errore(err)
		}
		if rowsAffected, _ := res.RowsAffected(); rowsAffected > 0 {
			AuditOperation("expire-maintenance", nil, fmt.Sprintf("Expired bounded: %d", rowsAffected))
		}
	}
	{
		res, err := db.ExecOrchestrator(`
			update
				database_instance_maintenance
				left join node_health on (processing_node_hostname = node_health.hostname AND processing_node_token = node_health.token)
			set
				database_instance_maintenance.maintenance_active = NULL
			where
				node_health.last_seen_active IS NULL
				and explicitly_bounded = 0
			`,
		)
		if err != nil {
			return log.Errore(err)
		}
		if rowsAffected, _ := res.RowsAffected(); rowsAffected > 0 {
			AuditOperation("expire-maintenance", nil, fmt.Sprintf("Expired dead: %d", rowsAffected))
		}
	}

	return nil
}
