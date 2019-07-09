# Copyright (C) The Arvados Authors. All rights reserved.
#
# SPDX-License-Identifier: AGPL-3.0

class PipelineTemplatesController < ApplicationController
  skip_around_action :require_thread_api_token, if: proc { |ctrl|
    !Rails.configuration.Users.AnonymousUserToken.empty? and
    'show' == ctrl.action_name
  }

  include PipelineComponentsHelper

  def show
    @objects = PipelineInstance.where(pipeline_template_uuid: @object.uuid)
    super
  end

  def show_pane_list
    %w(Components Pipelines Advanced)
  end
end
