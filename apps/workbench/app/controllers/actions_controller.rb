class ActionsController < ApplicationController

  @@exposed_actions = {}
  def self.expose_action method, &block
    @@exposed_actions[method] = true
    define_method method, block
  end

  def model_class
    ArvadosBase::resource_class_for_uuid(params[:uuid])
  end

  def show
    @object = model_class.andand.find(params[:uuid])
    if @object.is_a? Link and
        @object.link_class == 'name' and
        ArvadosBase::resource_class_for_uuid(@object.head_uuid) == Collection
      redirect_to collection_path(id: @object.uuid)
    elsif @object
      redirect_to @object
    else
      raise ActiveRecord::RecordNotFound
    end
  end

  def post
    params.keys.collect(&:to_sym).each do |param|
      if @@exposed_actions[param]
        return self.send(param)
      end
    end
    redirect_to :back
  end

  expose_action :copy_selections_into_project do
    move_or_copy :copy
  end

  expose_action :move_selections_into_project do
    move_or_copy :move
  end

  def move_or_copy action
    uuids_to_add = params["selection"]
    uuids_to_add = [ uuids_to_add ] unless uuids_to_add.is_a? Array
    uuids_to_add.
      collect { |x| ArvadosBase::resource_class_for_uuid(x) }.
      uniq.
      each do |resource_class|
      resource_class.filter([['uuid','in',uuids_to_add]]).each do |src|
        if resource_class == Collection and not Collection.attribute_info.include?(:name)
          dst = Link.new(owner_uuid: @object.uuid,
                         tail_uuid: @object.uuid,
                         head_uuid: src.uuid,
                         link_class: 'name',
                         name: src.uuid)
        else
          case action
          when :copy
            dst = src.dup
            if dst.respond_to? :'name='
              if dst.name
                dst.name = "Copy of #{dst.name}"
              else
                dst.name = "Copy of unnamed #{dst.class_for_display.downcase}"
              end
            end
            if resource_class == Collection
              dst.manifest_text = Collection.select([:manifest_text]).where(uuid: src.uuid).first.manifest_text
            end
          when :move
            dst = src
          else
            raise ArgumentError.new "Unsupported action #{action}"
          end
          dst.owner_uuid = @object.uuid
          dst.tail_uuid = @object.uuid if dst.class == Link
        end
        begin
          dst.save!
        rescue
          dst.name += " (#{Time.now.localtime})" if dst.respond_to? :name=
          dst.save!
        end
      end
    end
    redirect_to @object
  end

  def arv_normalize mt, *opts
    r = ""
    IO.popen(['arv-normalize'] + opts, 'w+b') do |io|
      io.write mt
      io.close_write
      while buf = io.read(2**16)
        r += buf
      end
    end
    r
  end

  expose_action :combine_selected_files_into_collection do
    lst = []
    files = []
    params["selection"].each do |s|
      a = ArvadosBase::resource_class_for_uuid s
      m = nil
      if a == Link
        begin
          m = CollectionsHelper.match(Link.find(s).head_uuid)
        rescue
        end
      else
        m = CollectionsHelper.match(s)
      end

      if m and m[1] and m[2]
        lst.append(m[1] + m[2])
        files.append(m)
      end
    end

    collections = Collection.where(uuid: lst)

    chash = {}
    collections.each do |c|
      c.reload()
      chash[c.uuid] = c
    end

    combined = ""
    files.each do |m|
      mt = chash[m[1]+m[2]].manifest_text
      if m[4]
        combined += arv_normalize mt, '--extract', m[4][1..-1]
      else
        combined += chash[m[1]+m[2]].manifest_text
      end
    end

    normalized = arv_normalize combined
    newc = Collection.new({:manifest_text => normalized})
    newc.save!

    chash.each do |k,v|
      l = Link.new({
                     tail_uuid: k,
                     head_uuid: newc.uuid,
                     link_class: "provenance",
                     name: "provided"
                   })
      l.save!
    end

    redirect_to controller: 'collections', action: :show, id: newc.uuid
  end

end
