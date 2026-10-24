create table rcd_host_setup (
    project_id integer not null references project(id) on delete cascade,
    id varchar(64) not null,
    request_key varchar(128) not null,
    request_hash varchar(64) not null,
    address varchar(64) not null,
    ssh_port integer not null,
    body text not null,
    revision integer not null,
    task_id integer not null default 0,
    template_id integer not null default 0,
    state varchar(40) not null,
    created_at varchar(40) not null,
    primary key (project_id, id),
    unique (project_id, request_key)
);
create index rcd_host_setup_task on rcd_host_setup(project_id, task_id);
create index rcd_host_setup_template on rcd_host_setup(project_id, template_id);
create table rcd_host_setup_active (
    project_id integer not null references project(id) on delete cascade,
    address varchar(64) not null,
    ssh_port integer not null,
    setup_id varchar(64) not null,
    primary key (project_id, address)
);
