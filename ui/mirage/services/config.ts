import { ConfigGetRequest, ConfigGetResponse, ConfigSetRequest, ConfigSetResponse } from 'waypoint-pb';
import { Request, Response } from 'miragejs';
import { decode } from '../helpers/protobufs';

// eslint-disable-next-line @typescript-eslint/explicit-module-boundary-types, @typescript-eslint/no-explicit-any
export function get(schema: any, { requestBody }: Request): Response {
  let requestMsg = decode(ConfigGetRequest, requestBody);
  let projectName = requestMsg.getProject().getProject();
  let project = schema.projects.findBy({ name: projectName });
  let variables = schema.configVariables.where({ projectId: project.id }).models;
  // The API returns config variables sorted alphabetically by name
  variables.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  let variablesList = variables.map((m) => m.toProtobuf());
  let response = new ConfigGetResponse();

  response.setVariablesList(variablesList);

  return this.serialize(response, 'application');
}

export function set(schema: any, { requestBody }: Request): Response {
  let requestMsg = decode(ConfigSetRequest, requestBody);
  let vars = requestMsg.toObject().variablesList;
  vars.forEach((v) => {
    let projName = v.project?.project;
    v.project = schema.projects.findBy({ name: projName });
    schema.configVariables.create(v);
  });

  let response = new ConfigSetResponse();

  return this.serialize(response, 'application');
}
