package access

// ProjectGrantee is one subject granted a project instance, with the name
// the hub holds for it, so an operator reads names beside IDs. A team
// profile's name is the one aicrewd registered, empty until it registers.
type ProjectGrantee struct {
	Kind      string `json:"kind"` // user, group or team_profile
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	ServiceID string `json:"service_id,omitempty"`
	TeamID    string `json:"team_id,omitempty"`
}

// ProjectGrantees lists every user, group and team profile granted the
// project instance, users first, then groups, then team profiles, each in
// name or team order. A disabled user or profile is listed and marked: the
// grant stays on record while the subject cannot use it.
func (s *Store) ProjectGrantees(instance string) ([]ProjectGrantee, error) {
	out := []ProjectGrantee{}
	if instance == "" {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT 0, 'user', u.id, u.name, u.disabled, '', '' FROM grants g JOIN users u ON g.kind='user' AND u.id=g.subject WHERE g.project=?
UNION ALL SELECT 1, 'group', a.id, a.name, 0, '', '' FROM grants g JOIN access_groups a ON g.kind='group' AND a.id=g.subject WHERE g.project=?
UNION ALL SELECT 2, 'team_profile', p.id, p.team_name, p.disabled, p.service_id, p.team_id FROM team_profile_grants g JOIN team_access_profiles p ON p.id=g.profile_id WHERE g.project=?
ORDER BY 1, 4, 6, 7, 3`, instance, instance, instance)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g ProjectGrantee
		var rank int
		if err := rows.Scan(&rank, &g.Kind, &g.ID, &g.Name, &g.Disabled, &g.ServiceID, &g.TeamID); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
