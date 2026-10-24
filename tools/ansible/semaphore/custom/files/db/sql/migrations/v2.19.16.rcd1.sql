create table rcd_package (
    project_id integer not null references project(id) on delete cascade,
    id varchar(64) not null,
    component varchar(16) not null,
    release_id varchar(128) not null,
    version varchar(128) not null,
    target varchar(32) not null,
    manifest text not null,
    archive_sha256 varchar(64) not null,
    archive_size bigint not null,
    enabled integer not null default 1,
    revision integer not null default 1,
    uploaded_at varchar(40) not null,
    uploaded_by varchar(255) not null,
    primary key (project_id, id),
    unique (project_id, component, release_id)
);

create table rcd_package_event (
    project_id integer not null,
    id varchar(64) not null,
    package_id varchar(64) not null,
    action varchar(32) not null,
    actor varchar(255) not null,
    created_at varchar(40) not null,
    primary key (project_id, id),
    foreign key (project_id, package_id) references rcd_package(project_id, id) on delete cascade
);

create index rcd_package_list on rcd_package(project_id, component, uploaded_at);
