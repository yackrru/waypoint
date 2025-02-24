import { Model, belongsTo, hasMany } from 'miragejs';
import { StatusReport } from 'waypoint-pb';
import MirageDeployment from './deployment';
import MirageRelease from './release';

export default Model.extend({
  application: belongsTo(),
  workspace: belongsTo(),
  target: belongsTo({ polymorphic: true }),
  status: belongsTo({ inverse: 'owner' }),
  health: belongsTo({ inverse: 'statusReport' }),
  resourcesHealthList: hasMany('health', { inverse: 'statusReportList' }),

  toProtobuf(): StatusReport {
    let result = new StatusReport();

    result.setApplication(this.application?.toProtobufRef());
    result.setWorkspace(this.workspace?.toProtobufRef());

    if (this.target instanceof MirageDeployment) {
      result.setDeploymentId(this.target.id);
    } else if (this.target instanceof MirageRelease) {
      result.setReleaseId(this.target.id);
    }
    result.setStatus(this.status?.toProtobuf());
    result.setId(this.id);
    // TODO: result.setStatusReport(value?: google_protobuf_any_pb.Any)
    result.setHealth(this.health?.toProtobuf());
    result.setDeprecatedResourcesHealthList(this.resourcesHealthList.models.map((h) => h.toProtobuf()));

    return result;
  },
});
