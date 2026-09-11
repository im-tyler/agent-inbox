# Groups — more than one supervisor — agent-inbox

A supervisor's context is rebuilt from scratch on every turn out of its fleet's
status lines and the notes that mention them. That is what makes supervision
accurate, and it is also what degrades as the fleet grows: seven projects means
seven status lines and every note about any of them, on every message you send.

Groups split the fleet. Each one gets a supervisor of its own and a tab of its
own, so two conversations stay about different things.

```json
"groups": [
  { "name": "infra",   "projects": ["teploy", "infra"] },
  { "name": "product", "projects": ["neutron", "fylun"] }
]
```

```
╭──────────────────────────────────────────────────────────────╮
│ agent-inbox                                                  │
│ infra 1●  ·  product ⠋                                       │
│ infra  claude                              fleet             │
│                                            ★ supervisor-infra│
│                                              teploy        ● │
│                                              infra         · │
│                                            2 projects        │
│                                            1 waiting         │
```

Each group's supervisor is provisioned the same way the single one is — named
`supervisor-<group>`, in a folder of its own. Override any of that with a
`king` block inside the group (`name`, `tool`, `dir`).

A project belongs to exactly one group, and validation rejects a config where
one is claimed twice. A project no group names — including one added later from
the dashboard — joins the first group, so a project is never left in the fleet
with no supervisor able to see it.

`shift+tab` cycles tabs from the composer; `[` and `]` (or `h`/`l`) do it with
the fleet focused. Each tab carries its own count of what is waiting, so a
project needing you in a tab you do not have open still says so.

Omit `groups` entirely for one supervisor over everything, which is the default
and what most installs want.
